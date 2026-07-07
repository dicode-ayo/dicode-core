package registry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Reconciler fans-in events from multiple sources and applies them to the registry.
type Reconciler struct {
	registry *Registry
	sources  []source.Source
	dataDir  string
	log      *zap.Logger

	// OnRegister is called after a task is registered (used by trigger engine).
	// It receives the registered task as a task.Kinded; consumers that only act
	// on kind: Task type-assert to *task.Spec.
	OnRegister func(k task.Kinded)
	// OnUnregister is called after a task is removed.
	OnUnregister func(id string)

	// runtime state — set when Run is called
	mu      sync.Mutex
	merged  chan source.Event
	cancels map[string]context.CancelFunc // sourceID → cancel fn
	runCtx  context.Context

	// readyCh is closed once Run has applied the initial inventory emitted by
	// every configured source. Until then a task lookup may miss a task that is
	// about to register, so task-scoped CLI commands wait on it (issue #464).
	readyCh   chan struct{}
	readyOnce sync.Once

	// pending holds events that failed validateTaskProviders because a
	// provider task was not yet registered. After any successful registration
	// these are re-emitted to merged for a retry. Keyed by task ID so a
	// repeated failure for the same task replaces the previous attempt
	// (always retrying the most recent event).
	pending map[string]source.Event
}

// NewReconciler creates a Reconciler for the given registry and sources.
// dataDir is the daemon's data directory (config.DataDir); it is injected as
// the ${DATADIR} template variable when loading task specs so buildin tasks
// can reference shared paths under the data dir.
func NewReconciler(r *Registry, sources []source.Source, dataDir string, log *zap.Logger) *Reconciler {
	return &Reconciler{
		registry: r,
		sources:  sources,
		dataDir:  dataDir,
		log:      log,
		cancels:  make(map[string]context.CancelFunc),
		pending:  make(map[string]source.Event),
		readyCh:  make(chan struct{}),
	}
}

// Run starts all sources and processes their events until ctx is cancelled.
func (rc *Reconciler) Run(ctx context.Context) error {
	rc.mu.Lock()
	rc.runCtx = ctx
	rc.merged = make(chan source.Event, 64)
	rc.mu.Unlock()

	// Source.Start emits its initial inventory synchronously into the source's
	// own channel before returning. Drain and apply that burst here — before
	// marking the reconciler ready — so a CLI task lookup arriving the instant
	// the control socket opens cannot observe a task that is about to register
	// (issue #464). The ongoing watcher is forwarded only after the initial
	// drain, so first-sync events are never raced by a later change.
	for _, src := range rc.sources {
		ch, err := rc.beginSource(src)
		if err != nil {
			return fmt.Errorf("start source %s: %w", src.ID(), err)
		}
		rc.drainInitial(ch)
		rc.forward(ctx, ch)
	}
	rc.markReady()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-rc.merged:
			rc.handle(ev)
		}
	}
}

// Ready returns a channel closed once Run has applied the initial inventory of
// every configured source. A daemon with no sources is ready immediately.
func (rc *Reconciler) Ready() <-chan struct{} { return rc.readyCh }

// markReady closes readyCh exactly once.
func (rc *Reconciler) markReady() {
	rc.readyOnce.Do(func() { close(rc.readyCh) })
}

// WaitReady blocks until the first sync completes, ctx is cancelled, or timeout
// elapses, reporting whether the reconciler became ready. A non-positive
// timeout waits only on the already-completed case and ctx.
func (rc *Reconciler) WaitReady(ctx context.Context, timeout time.Duration) bool {
	select {
	case <-rc.readyCh:
		return true
	default:
	}
	if timeout <= 0 {
		select {
		case <-rc.readyCh:
			return true
		case <-ctx.Done():
			return false
		}
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-rc.readyCh:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

// AddSource adds a new source at runtime and starts it immediately.
// Safe to call from any goroutine after Run has been called.
func (rc *Reconciler) AddSource(src source.Source) error {
	return rc.startSource(src)
}

// RemoveSource stops and removes a source by its ID.
// Safe to call from any goroutine after Run has been called.
func (rc *Reconciler) RemoveSource(id string) {
	rc.mu.Lock()
	cancel, ok := rc.cancels[id]
	delete(rc.cancels, id)
	rc.mu.Unlock()
	if ok {
		cancel()
	}
}

// startSource begins watching a source and forwarding its events to merged.
func (rc *Reconciler) startSource(src source.Source) error {
	rc.mu.Lock()
	ctx := rc.runCtx
	rc.mu.Unlock()
	ch, err := rc.beginSource(src)
	if err != nil {
		return err
	}
	rc.forward(ctx, ch)
	return nil
}

// beginSource starts a source and registers its cancel func, returning the
// event channel without forwarding it. Callers that need the initial burst
// applied before ongoing events (Run) drain the channel themselves first.
func (rc *Reconciler) beginSource(src source.Source) (<-chan source.Event, error) {
	rc.mu.Lock()
	ctx := rc.runCtx
	rc.mu.Unlock()
	if ctx == nil {
		return nil, fmt.Errorf("reconciler not yet running")
	}

	srcCtx, cancel := context.WithCancel(ctx)
	ch, err := src.Start(srcCtx)
	if err != nil {
		cancel()
		return nil, err
	}

	rc.mu.Lock()
	rc.cancels[src.ID()] = cancel
	rc.mu.Unlock()
	return ch, nil
}

// drainInitial applies every event already buffered on a freshly started
// source channel. Source.Start emits its initial inventory synchronously
// before returning, so a non-blocking drain captures exactly that burst
// without waiting on later changes.
func (rc *Reconciler) drainInitial(ch <-chan source.Event) {
	for {
		select {
		case ev := <-ch:
			rc.handle(ev)
		default:
			return
		}
	}
}

// forward pumps a source channel into the merged stream until ctx is done.
func (rc *Reconciler) forward(ctx context.Context, ch <-chan source.Event) {
	go func() {
		for ev := range ch {
			// Without the ctx select, a slow main loop plus a closed merged
			// reader during shutdown would block this goroutine forever
			// holding source events. ctx is the parent context, not the
			// per-source srcCtx, so this drains naturally on full reconciler
			// shutdown rather than per-source cancellation.
			select {
			case rc.merged <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (rc *Reconciler) handle(ev source.Event) {
	switch ev.Kind {
	case source.EventAdded, source.EventUpdated:
		var k task.Kinded
		if ev.Kinded != nil {
			// TaskSet sources pre-resolve the task (overrides already applied).
			k = ev.Kinded
		} else {
			extras := ev.ExtraVars
			if extras == nil {
				extras = make(map[string]string, 1)
			}
			if _, ok := extras[task.VarDataDir]; !ok && rc.dataDir != "" {
				// Don't clobber a source-supplied DATADIR (allows tests to override).
				// Clone before mutate — ev.ExtraVars may be shared across event consumers.
				cloned := make(map[string]string, len(extras)+1)
				for key, v := range extras {
					cloned[key] = v
				}
				cloned[task.VarDataDir] = rc.dataDir
				extras = cloned
			}
			loaded, err := task.LoadKindedDir(ev.TaskDir, extras)
			if err != nil {
				rc.log.Warn("failed to load task",
					zap.String("task", ev.TaskID),
					zap.String("source", ev.Source),
					zap.Error(err),
				)
				return
			}
			k = loaded
		}
		// The registry keys on the event's TaskID. Flat git/local sources already
		// load with ID == basename == TaskID, but namespaced/pre-resolved tasks
		// need the canonical ID stamped here so every layer agrees.
		k.SetTaskID(ev.TaskID)
		for _, w := range k.LoadWarnings() {
			rc.log.Warn("task config warning",
				zap.String("task", ev.TaskID),
				zap.String("source", ev.Source),
				zap.String("warning", w),
			)
		}
		// Provider validation (issue #119) only applies to kind: Task env entries;
		// pipelines declare no env providers.
		if spec, ok := k.(*task.Spec); ok {
			if err := rc.validateTaskProviders(spec); err != nil {
				rc.log.Warn("task references unknown provider; queued for retry after next registration",
					zap.String("task", ev.TaskID),
					zap.String("source", ev.Source),
					zap.Error(err),
				)
				rc.mu.Lock()
				rc.pending[ev.TaskID] = ev
				rc.mu.Unlock()
				return
			}
		}
		if err := rc.registry.Register(k); err != nil {
			rc.log.Error("failed to register task", zap.String("task", ev.TaskID), zap.Error(err))
			return
		}
		// Clear from pending if this was a successful retry.
		rc.mu.Lock()
		delete(rc.pending, ev.TaskID)
		rc.mu.Unlock()
		rc.log.Info("task registered",
			zap.String("task", ev.TaskID),
			zap.String("kind", string(ev.Kind)),
		)
		if rc.OnRegister != nil {
			rc.OnRegister(k)
		}
		// Re-queue pending tasks — any of them may now have their provider satisfied.
		rc.retryPending()

	case source.EventRemoved:
		// Cancel any pending retry so a removed-then-retried task is not
		// re-registered after its provider eventually shows up.
		rc.mu.Lock()
		delete(rc.pending, ev.TaskID)
		rc.mu.Unlock()
		rc.registry.Unregister(ev.TaskID)
		rc.log.Info("task unregistered", zap.String("task", ev.TaskID))
		if rc.OnUnregister != nil {
			rc.OnUnregister(ev.TaskID)
		}
	}
}

// retryPending re-emits every pending event to the merged channel so they
// get a second chance now that a new provider may have registered. It runs
// in a goroutine to avoid deadlocking the handle() call-site (handle is
// called from the Run loop, which is the only reader of merged).
func (rc *Reconciler) retryPending() {
	rc.mu.Lock()
	if len(rc.pending) == 0 {
		rc.mu.Unlock()
		return
	}
	events := make([]source.Event, 0, len(rc.pending))
	for _, ev := range rc.pending {
		events = append(events, ev)
	}
	ctx := rc.runCtx
	rc.mu.Unlock()

	go func() {
		for _, ev := range events {
			// Re-check pending membership: an EventRemoved may have arrived
			// after the snapshot was taken, deleting this task from rc.pending
			// and unregistering it. Skipping here prevents a re-registration
			// of a task that was intentionally removed.
			rc.mu.Lock()
			_, stillPending := rc.pending[ev.TaskID]
			rc.mu.Unlock()
			if !stillPending {
				continue
			}
			select {
			case rc.merged <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// validateTaskProviders inspects every EnvEntry whose From has the
// "task:" prefix and confirms the referenced provider task is already
// registered. Issue #119: a misspelled provider must not silently fall
// through to a runtime spawn failure on every consumer launch.
//
// Order dependency: provider tasks must reconcile before their consumers.
// The buildin source registers providers first because they live under
// tasks/buildin/secret-providers/* and the taskset.yaml entry order is
// preserved. For multi-source setups, a miss on the first reconciler pass
// causes the consumer to be queued in rc.pending and retried after the
// next successful registration (see handle and retryPending).
func (rc *Reconciler) validateTaskProviders(spec *task.Spec) error {
	for _, e := range spec.Permissions.Env {
		kind, target := task.ParseFrom(e.From)
		if kind != task.FromKindTask {
			continue
		}
		if target == "" {
			return fmt.Errorf("env entry %q: from: task: target is empty", e.Name)
		}
		if _, ok := rc.registry.Get(target); !ok {
			return fmt.Errorf("env entry %q: provider task %q not registered", e.Name, target)
		}
	}
	return nil
}
