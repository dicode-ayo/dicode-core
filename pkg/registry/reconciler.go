package registry

import (
	"context"
	"fmt"
	"sync"

	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// initialSyncBarrier is an internal sentinel event kind injected by the
// reconciler itself — never emitted by real sources. Each initially-configured
// source's forwarding goroutine pushes one barrier into the merged channel
// right after the source's initial scan batch, so by the time the Run loop
// dequeues the barrier every task from that batch has been handled (registered
// or rejected). Once all barriers have been seen the Ready channel closes.
const initialSyncBarrier source.EventKind = "dicode-internal/initial-sync-barrier"

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

	// pending holds events that failed validateTaskProviders because a
	// provider task was not yet registered. After any successful registration
	// these are re-emitted to merged for a retry. Keyed by task ID so a
	// repeated failure for the same task replaces the previous attempt
	// (always retrying the most recent event).
	pending map[string]source.Event

	// ready closes once the first sync completes: every initially-configured
	// source has started AND its initial scan batch has been handled (#464).
	// It never closes when the run context is cancelled first, so consumers
	// must select on their own timeout/context alongside it. initPending
	// (guarded by mu) counts sources whose barrier has not yet been seen.
	ready       chan struct{}
	readyOnce   sync.Once
	initPending int
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
		ready:    make(chan struct{}),
	}
}

// Ready returns a channel that closes once the reconciler's first sync has
// completed — every initially-configured source has started and the tasks
// from its initial scan have been registered (or rejected). The channel never
// closes if the run context is cancelled before the first sync finishes, so
// consumers must select on their own timeout/context alongside it.
func (rc *Reconciler) Ready() <-chan struct{} { return rc.ready }

// markInitialSourceSynced records one source's initial-sync barrier passing
// through the Run loop; when the last one lands, Ready closes.
func (rc *Reconciler) markInitialSourceSynced() {
	rc.mu.Lock()
	rc.initPending--
	done := rc.initPending <= 0
	rc.mu.Unlock()
	if done {
		rc.readyOnce.Do(func() { close(rc.ready) })
	}
}

// Run starts all sources and processes their events until ctx is cancelled.
func (rc *Reconciler) Run(ctx context.Context) error {
	rc.mu.Lock()
	rc.runCtx = ctx
	rc.merged = make(chan source.Event, 64)
	rc.initPending = len(rc.sources)
	rc.mu.Unlock()

	if len(rc.sources) == 0 {
		// Nothing to sync — the first sync is trivially complete.
		rc.readyOnce.Do(func() { close(rc.ready) })
		// Still need to run so dynamic AddSource works.
		goto loop
	}

	for _, src := range rc.sources {
		if err := rc.startSource(src, true); err != nil {
			return fmt.Errorf("start source %s: %w", src.ID(), err)
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-rc.merged:
			if ev.Kind == initialSyncBarrier {
				rc.markInitialSourceSynced()
				continue
			}
			rc.handle(ev)
		}
	}
}

// AddSource adds a new source at runtime and starts it immediately.
// Safe to call from any goroutine after Run has been called.
// Runtime-added sources never participate in the first-sync readiness
// barrier — Ready tracks only the initially-configured set.
func (rc *Reconciler) AddSource(src source.Source) error {
	return rc.startSource(src, false)
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
//
// trackInitial marks the source as part of the first-sync readiness barrier:
// sources emit their initial scan synchronously inside Start (into the
// buffered channel), so len(ch) captured here is the initial batch size. The
// forwarding goroutine pushes an initialSyncBarrier sentinel into merged right
// after that batch; when the Run loop dequeues it, every initial task from
// this source has already been handled. Any watch-phase events that slipped
// into the buffer before the snapshot only inflate the count — they are
// already buffered, so the barrier still lands without waiting on future
// activity.
func (rc *Reconciler) startSource(src source.Source, trackInitial bool) error {
	rc.mu.Lock()
	ctx := rc.runCtx
	rc.mu.Unlock()
	if ctx == nil {
		return fmt.Errorf("reconciler not yet running")
	}

	srcCtx, cancel := context.WithCancel(ctx)
	ch, err := src.Start(srcCtx)
	if err != nil {
		cancel()
		return err
	}
	initialBatch := 0
	if trackInitial {
		initialBatch = len(ch)
	}

	rc.mu.Lock()
	rc.cancels[src.ID()] = cancel
	rc.mu.Unlock()

	go func() {
		// sendBarrier signals this source's initial batch has been forwarded.
		// It reports false when ctx is done, so the forwarder can bail out.
		// On shutdown before the barrier lands, Ready simply never closes —
		// readiness consumers select on their own timeout/context.
		sendBarrier := func() bool {
			select {
			case rc.merged <- source.Event{Kind: initialSyncBarrier}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if trackInitial && initialBatch == 0 {
			if !sendBarrier() {
				return
			}
			trackInitial = false
		}
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
			if trackInitial {
				if initialBatch--; initialBatch == 0 {
					if !sendBarrier() {
						return
					}
					trackInitial = false
				}
			}
		}
	}()
	return nil
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
				// Warn only on the transition into "queued"; every successful
				// registration re-runs this check for all still-pending tasks,
				// so warning each time turns one unresolved dependency into N
				// identical lines during startup (#521). Repeat re-checks of an
				// already-queued task drop to debug — the signal is preserved
				// (a task that resolves is deleted from pending, and a later
				// re-failure warns again as a fresh transition).
				rc.mu.Lock()
				_, alreadyQueued := rc.pending[ev.TaskID]
				rc.pending[ev.TaskID] = ev
				rc.mu.Unlock()
				fields := []zap.Field{
					zap.String("task", ev.TaskID),
					zap.String("source", ev.Source),
					zap.Error(err),
				}
				if alreadyQueued {
					rc.log.Debug("task still references unknown provider; remains queued for retry", fields...)
				} else {
					rc.log.Warn("task references unknown provider; queued for retry after next registration", fields...)
				}
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
