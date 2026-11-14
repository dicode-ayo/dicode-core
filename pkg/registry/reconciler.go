package registry

import (
	"context"
	"errors"
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

// sourceRemoved is an internal marker the Run loop handles by deregistering
// every task owned by the marker's source generation.
const sourceRemoved source.EventKind = "dicode-internal/source-removed"

// ErrDuplicateSource is returned by AddSource when a source with the same ID
// is already running or starting.
var ErrDuplicateSource = errors.New("source with this id is already running")

// sourceEvent is a source event tagged with the generation of the source
// run that produced it. gen 0 marks an event with no owning source run.
// Event.Source is always the producing source's ID.
type sourceEvent struct {
	source.Event
	gen uint64
	// done is closed by the Run loop after it handles a sourceRemoved marker.
	done chan struct{}
}

// runningSource is one started (or starting) source. gen distinguishes it
// from an earlier or later source with the same ID.
type runningSource struct {
	src    source.Source
	cancel context.CancelFunc
	gen    uint64
	// done closes once Start has returned and the forwarder has exited.
	done chan struct{}
}

// Reconciler fans-in events from multiple sources and applies them to the registry.
type Reconciler struct {
	registry *Registry
	sources  []source.Source
	log      *zap.Logger

	// OnRegister is called after a task is registered (used by trigger engine).
	// It receives the registered task as a task.Kinded; consumers that only act
	// on kind: Task type-assert to *task.Spec.
	OnRegister func(k task.Kinded)
	// OnUnregister is called after a task is removed.
	OnUnregister func(id string)

	// runtime state — set when Run is called
	mu      sync.Mutex
	merged  chan sourceEvent
	cancels map[string]*runningSource // sourceID → running source, set before Start
	lastGen uint64
	runCtx  context.Context

	// owners maps a registered task ID to the generation of the source run
	// that registered it. Only the Run loop writes it.
	owners map[string]uint64

	// pending holds events that failed validateTaskProviders because a
	// provider task was not yet registered. After any successful registration
	// these are re-emitted to merged for a retry. Keyed by task ID so a
	// repeated failure for the same task replaces the previous attempt
	// (always retrying the most recent event).
	pending map[string]sourceEvent

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
func NewReconciler(r *Registry, sources []source.Source, log *zap.Logger) *Reconciler {
	return &Reconciler{
		registry: r,
		sources:  sources,
		log:      log,
		cancels:  make(map[string]*runningSource),
		owners:   make(map[string]uint64),
		pending:  make(map[string]sourceEvent),
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
	rc.merged = make(chan sourceEvent, 64)
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
			switch ev.Kind {
			case initialSyncBarrier:
				rc.markInitialSourceSynced()
			case sourceRemoved:
				rc.unregisterOwned(ev.gen)
				close(ev.done)
			default:
				rc.handle(ev)
			}
		}
	}
}

// AddSource adds a new source at runtime and starts it immediately.
// Safe to call from any goroutine after Run has been called.
// Runtime-added sources never participate in the first-sync readiness
// barrier — Ready tracks only the initially-configured set.
//
// It returns ErrDuplicateSource while a source with the same ID is running or
// starting, and nil when RemoveSource cancelled the source mid-start (the
// source is already gone).
func (rc *Reconciler) AddSource(src source.Source) error {
	return rc.startSource(src, false)
}

// RemoveSource stops a source by its ID, deregisters every task it
// registered (calling OnUnregister for each), waits for its Start call and
// event forwarding to finish, then calls Remove on sources implementing
// source.Remover. It returns once the tasks are deregistered.
// A source still inside Start is cancelled and never forwards events.
//
// Safe to call from any goroutine after Run has been called, except from
// OnRegister/OnUnregister, which run on the Run loop it waits for.
func (rc *Reconciler) RemoveSource(id string) {
	rc.removeSource(id, nil)
}

// RemoveSourceIfCurrent is RemoveSource for src.ID(), but only while src is
// the instance running under that ID: a caller holding a source that was
// already removed cannot tear down a newer source that reused the ID.
func (rc *Reconciler) RemoveSourceIfCurrent(src source.Source) {
	rc.removeSource(src.ID(), src)
}

func (rc *Reconciler) removeSource(id string, want source.Source) {
	rc.mu.Lock()
	rs, ok := rc.cancels[id]
	if !ok || (want != nil && rs.src != want) {
		rc.mu.Unlock()
		return
	}
	delete(rc.cancels, id)
	for taskID, ev := range rc.pending {
		if ev.gen == rs.gen {
			delete(rc.pending, taskID)
		}
	}
	ctx := rc.runCtx
	rc.mu.Unlock()
	rs.cancel()

	marker := sourceEvent{Event: source.Event{Kind: sourceRemoved, Source: id}, gen: rs.gen, done: make(chan struct{})}
	select {
	case rc.merged <- marker:
	case <-ctx.Done():
		return
	}
	for _, done := range []chan struct{}{marker.done, rs.done} {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
	if r, ok := rs.src.(source.Remover); ok {
		r.Remove()
	}
}

// unregisterOwned deregisters every task registered by source run gen.
// Runs on the Run loop.
func (rc *Reconciler) unregisterOwned(gen uint64) {
	var ids []string
	for taskID, owner := range rc.owners {
		if owner == gen {
			ids = append(ids, taskID)
		}
	}
	for _, taskID := range ids {
		rc.unregister(taskID)
	}
}

func (rc *Reconciler) unregister(taskID string) {
	rc.mu.Lock()
	delete(rc.pending, taskID)
	rc.mu.Unlock()
	delete(rc.owners, taskID)
	rc.registry.Unregister(taskID)
	rc.log.Info("task unregistered", zap.String("task", taskID))
	if rc.OnUnregister != nil {
		rc.OnUnregister(taskID)
	}
}

// live reports whether ev comes from a source run that is still registered.
func (rc *Reconciler) live(ev sourceEvent) bool {
	if ev.gen == 0 {
		return true
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rs, ok := rc.cancels[ev.Source]
	return ok && rs.gen == ev.gen
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
	id := src.ID()
	rc.mu.Lock()
	ctx := rc.runCtx
	if ctx == nil {
		rc.mu.Unlock()
		return fmt.Errorf("reconciler not yet running")
	}
	if _, dup := rc.cancels[id]; dup {
		rc.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrDuplicateSource, id)
	}
	srcCtx, cancel := context.WithCancel(ctx)
	rc.lastGen++
	rs := &runningSource{src: src, cancel: cancel, gen: rc.lastGen, done: make(chan struct{})}
	rc.cancels[id] = rs
	rc.mu.Unlock()

	ch, err := src.Start(srcCtx)

	rc.mu.Lock()
	removed := rc.cancels[id] != rs
	if err != nil && !removed {
		delete(rc.cancels, id)
	}
	rc.mu.Unlock()
	if removed {
		// RemoveSource cancelled this start; its events must never register.
		close(rs.done)
		if trackInitial {
			rc.markInitialSourceSynced()
		}
		return nil
	}
	if err != nil {
		cancel()
		close(rs.done)
		return err
	}
	initialBatch := 0
	if trackInitial {
		initialBatch = len(ch)
	}

	go func() {
		defer close(rs.done)
		// sendBarrier signals this source's initial batch has been forwarded.
		// It reports false when ctx is done, so the forwarder can bail out.
		// On shutdown before the barrier lands, Ready simply never closes —
		// readiness consumers select on their own timeout/context.
		sendBarrier := func() bool {
			select {
			case rc.merged <- sourceEvent{Event: source.Event{Kind: initialSyncBarrier}}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		// A source removed or closed before its initial batch drained still
		// counts as synced.
		defer func() {
			if trackInitial {
				sendBarrier()
			}
		}()
		if trackInitial && initialBatch == 0 {
			if !sendBarrier() {
				return
			}
			trackInitial = false
		}
		for {
			var ev source.Event
			var ok bool
			select {
			case ev, ok = <-ch:
				if !ok {
					return
				}
			case <-srcCtx.Done():
				return
			}
			// Without srcCtx, a stopped Run loop or a removal would block
			// this goroutine forever holding source events.
			ev.Source = id
			select {
			case rc.merged <- sourceEvent{Event: ev, gen: rs.gen}:
			case <-srcCtx.Done():
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

func (rc *Reconciler) handle(ev sourceEvent) {
	if !rc.live(ev) {
		rc.log.Debug("dropping event from removed source",
			zap.String("task", ev.TaskID), zap.String("source", ev.Source))
		return
	}
	switch ev.Kind {
	case source.EventAdded, source.EventUpdated:
		k := ev.Kinded
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
		if ev.gen != 0 {
			rc.owners[ev.TaskID] = ev.gen
		} else {
			delete(rc.owners, ev.TaskID)
		}
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
		rc.unregister(ev.TaskID)
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
	events := make([]sourceEvent, 0, len(rc.pending))
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
// dicode-buildin's secret-providers/* and the taskset.yaml entry order is
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
