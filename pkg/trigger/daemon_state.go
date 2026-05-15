package trigger

import (
	"context"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// beforeContains reports whether any BeforeEntry in entries names taskID.
// Pulls only the Task field; per-edge overrides are irrelevant for
// membership checks.
func beforeContains(entries []task.BeforeEntry, taskID string) bool {
	for _, e := range entries {
		if e.Task == taskID {
			return true
		}
	}
	return false
}

// DaemonState describes the preflight/lifecycle phase of a daemon task as
// observed by the trigger engine. Surfaced via Engine.DaemonState so the
// WebUI (and operators reading API responses) can see *why* a daemon isn't
// up — distinguishing "still waiting on the render task" from "render
// failed" from "explicitly stopped".
//
// The five values are intentionally a closed set rather than a bag of
// booleans. Adding a new phase should require an explicit case in any
// switch that consumes this type.
type DaemonState string

const (
	// DaemonStopped is the zero value. Returned for any task ID the engine
	// hasn't tracked yet — including non-daemon tasks and unknown IDs — so
	// callers don't have to special-case "not found".
	DaemonStopped DaemonState = "stopped"

	// DaemonPrereqRunning indicates the engine has dispatched the daemon's
	// trigger.before tasks and is waiting for them to complete.
	DaemonPrereqRunning DaemonState = "prereq_running"

	// DaemonPrereqFailed indicates at least one prereq finished with
	// status=failure (or was cancelled). The daemon was NOT started; the
	// engine surfaces the most recent prereq run for the operator to inspect.
	DaemonPrereqFailed DaemonState = "prereq_failed"

	// DaemonRunning indicates the daemon's container/process is up. Set
	// after preflight succeeds (or immediately, when before: is empty).
	DaemonRunning DaemonState = "running"

	// DaemonStopping indicates a restart is in flight — the engine is
	// tearing down the current run before re-firing preflight. Distinct
	// from DaemonStopped so operators can see the brief intermediate state
	// during a prereq-driven restart.
	DaemonStopping DaemonState = "stopping"
)

// daemonStateMap is a thread-safe taskID → DaemonState map. Kept as a
// dedicated type so we can swap in a fancier representation later (e.g.
// per-state event hooks) without touching every call site.
type daemonStateMap struct {
	mu sync.RWMutex
	m  map[string]DaemonState
}

func newDaemonStateMap() *daemonStateMap {
	return &daemonStateMap{m: make(map[string]DaemonState)}
}

func (d *daemonStateMap) get(taskID string) DaemonState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.m[taskID]
	if !ok {
		return DaemonStopped
	}
	return s
}

func (d *daemonStateMap) set(taskID string, s DaemonState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s == DaemonStopped {
		// Conserve memory: stopped is the default, no need to persist it.
		delete(d.m, taskID)
		return
	}
	d.m[taskID] = s
}

// DaemonState returns the current lifecycle phase of the daemon with the
// given task ID. Returns DaemonStopped for unknown / never-started tasks.
//
// Safe for concurrent use.
func (e *Engine) DaemonState(taskID string) DaemonState {
	return e.daemonStates.get(taskID)
}

// setDaemonState records the daemon's lifecycle phase. Internal helper:
// only the engine's daemon-management paths should call it. Setting state
// to DaemonStopped removes the entry from the map.
func (e *Engine) setDaemonState(taskID string, s DaemonState) {
	e.daemonStates.set(taskID, s)
}

// notifyPrereqCompletion is the post-success hook the engine calls from
// FireChain when ANY task finishes with status=success. It walks the
// registered daemons and queues a restart for each one that lists
// completedTaskID in its trigger.before.
//
// Coalescing: at most one restart per daemon is in flight at any time —
// concurrent prereq completions are dropped via restartGate.tryAcquire.
// This prevents thrash when a daemon depends on a prereq that's on a
// short cron (credential rotation, periodic config render, etc.).
func (e *Engine) notifyPrereqCompletion(completedTaskID string) {
	for _, spec := range e.registry.All() {
		if !spec.Trigger.Daemon {
			continue
		}
		if !beforeContains(spec.Trigger.Before, completedTaskID) {
			continue
		}
		// Only attempt a restart if the daemon was actually up. Restarting
		// a daemon we never managed to start (e.g. PrereqFailed on first
		// boot) would race with the in-flight first-boot path; the
		// prereq's success will fall through to the normal startDaemon
		// path via the next registration cycle.
		state := e.DaemonState(spec.ID)
		if state != DaemonRunning {
			e.log.Debug("prereq completion ignored — daemon not Running",
				zap.String("daemon", spec.ID),
				zap.String("prereq", completedTaskID),
				zap.String("state", string(state)),
			)
			continue
		}
		e.queueDaemonRestart(spec)
	}
}

// queueDaemonRestart performs the stop-then-start cycle for a daemon
// whose trigger.before has just been re-satisfied. Holds a per-daemon
// lock (sync.Map keyed on task ID) so a flurry of prereq completions
// produces at most ONE outstanding restart. Subsequent calls during a
// restart are silently dropped.
func (e *Engine) queueDaemonRestart(spec *task.Spec) {
	if !e.restartGates.tryAcquire(spec.ID) {
		// A restart is already queued or in flight; coalesce.
		e.log.Debug("daemon restart coalesced", zap.String("task", spec.ID))
		return
	}
	go func() {
		defer e.restartGates.release(spec.ID)

		e.log.Info("daemon restart triggered by prereq completion",
			zap.String("task", spec.ID),
		)
		e.setDaemonState(spec.ID, DaemonStopping)

		// Stop the existing run. KillRun is best-effort; the daemon may
		// already have exited between FireChain's success notification
		// and this goroutine waking up.
		e.daemonMu.Lock()
		runID := e.daemonRuns[spec.ID]
		e.daemonMu.Unlock()
		if runID != "" {
			e.KillRun(runID)
			// Wait briefly for the run to actually terminate so the
			// onDaemonRunFinished restart hook (which itself runs
			// startDaemon) doesn't race with our explicit restart.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = e.WaitRun(ctx, runID)
			cancel()
		}

		// Re-fire preflight + start. The onDaemonRunFinished path will
		// ALSO try to restart on cancellation in some configurations;
		// startDaemon's own idempotency (daemonRuns gating) keeps that
		// safe.
		e.startDaemon(spec)
	}()
}

// restartGate is a per-task at-most-one-in-flight lock. Implemented as a
// sync.Map keyed on task ID; values are unused (the presence/absence of
// the key is the lock state).
type restartGate struct {
	gates sync.Map // map[string]struct{} — presence == locked
}

func newRestartGate() *restartGate { return &restartGate{} }

// tryAcquire returns true if the caller now holds the lock for taskID,
// false if another goroutine already does.
func (g *restartGate) tryAcquire(taskID string) bool {
	_, loaded := g.gates.LoadOrStore(taskID, struct{}{})
	return !loaded
}

// release frees the lock for taskID. Must be called exactly once per
// successful tryAcquire.
func (g *restartGate) release(taskID string) {
	g.gates.Delete(taskID)
}
