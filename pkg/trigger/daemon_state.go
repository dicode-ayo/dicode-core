package trigger

import "sync"

// DaemonState describes the lifecycle phase of a daemon task as observed by
// the trigger engine. Surfaced via Engine.DaemonState so the WebUI (and
// operators reading API responses) can see *why* a daemon isn't up —
// distinguishing "running" from "failed to launch" from "crashed" from
// "explicitly stopped".
//
// The values are a closed set rather than a bag of booleans.
// Adding a new phase should require an explicit case in any switch that
// consumes this type.
type DaemonState string

const (
	// DaemonStopped is the zero value. Returned for any task ID the engine
	// hasn't tracked yet — including non-daemon tasks and unknown IDs — so
	// callers don't have to special-case "not found". Also the resting
	// state after a deliberate Unregister or engine shutdown, AND after a
	// daemon body exits cleanly (status=success) with no configured
	// auto-restart. See DaemonCrashed for the non-success counterpart.
	DaemonStopped DaemonState = "stopped"

	// DaemonRunning indicates the daemon's container/process is up. Set once
	// the daemon body launches successfully.
	DaemonRunning DaemonState = "running"

	// DaemonStopping indicates a restart is in flight — the engine is
	// tearing down the current run before re-firing the daemon. Distinct
	// from DaemonStopped so operators can see the brief intermediate state
	// during a restart.
	DaemonStopping DaemonState = "stopping"

	// DaemonFailedAfterPreflight indicates the daemon body's launch failed —
	// e.g. binary missing, port already bound, runtime resource exhaustion —
	// i.e. the fireAsync call to launch the body returned an error. Distinct
	// from DaemonStopped so operators can tell "daemon body broke" apart from
	// "operator deliberately stopped it / never started it". Reachable only
	// from startDaemon's launch-error branch (issue #318).
	//
	// The const name and wire value "failed_after_preflight" are kept as-is:
	// they are a live API/WebUI enum value, so renaming would be a breaking
	// change. The current meaning is simply "daemon body launch failed".
	DaemonFailedAfterPreflight DaemonState = "failed_after_preflight"

	// DaemonCrashed indicates the daemon's body successfully started but
	// then exited with any non-success status (failure, cancelled, etc.)
	// AND the configured restart policy will NOT restart it — i.e. either
	// restart=never, or restart=on-failure with a status the engine
	// doesn't treat as a failure. Reachable only from
	// onDaemonRunFinished's no-restart branch.
	//
	// Distinct from DaemonStopped (clean exit / operator-initiated) and
	// from DaemonFailedAfterPreflight (fireAsync-time error, before the
	// daemon body got to run) so operators can tell the three apart in
	// the WebUI / API.
	//
	// Terminal: like DaemonFailedAfterPreflight, an operator must re-fire
	// the daemon (e.g. via manual run or a registry reload) to retry. The
	// engine does not automatically transition out of this state.
	DaemonCrashed DaemonState = "crashed"

	// DaemonCrashLooping indicates the daemon's body has failed
	// crashloopThreshold consecutive starts, each dying within
	// crashloopSustainWindow (issue #458). Unlike DaemonCrashed the engine IS
	// still restarting the body (restart=always / on-failure), so a
	// point-in-time snapshot would intermittently land in the brief
	// spawn-before-crash window and report a hard-failing task as "running".
	// This state overrides that transient: while the crash-loop tracker is
	// tripped, DaemonState reports crashlooping regardless of whether a spawn
	// is momentarily in flight.
	//
	// Self-clearing: the state ends as soon as a run sustains past the
	// window, exits cleanly, is cancelled by an operator, or the task is
	// unregistered — see pkg/trigger/crashloop.go for the exact rule.
	DaemonCrashLooping DaemonState = "crashlooping"
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
// Crash-loop override (issue #458): while the crash-loop tracker is tripped
// for this task, the reported state is DaemonCrashLooping even if the
// underlying map says DaemonRunning — a crash-looping daemon's respawns set
// DaemonRunning for the brief spawn-before-crash window, and surfacing that
// transient would present a hard-failing task as healthy.
//
// Safe for concurrent use.
func (e *Engine) DaemonState(taskID string) DaemonState {
	if e.crashloops.isCrashLooping(taskID) {
		return DaemonCrashLooping
	}
	return e.daemonStates.get(taskID)
}

// setDaemonState records the daemon's lifecycle phase. Internal helper:
// only the engine's daemon-management paths should call it. Setting state
// to DaemonStopped removes the entry from the map.
func (e *Engine) setDaemonState(taskID string, s DaemonState) {
	e.daemonStates.set(taskID, s)
}

// daemonRegistered reports whether taskID is currently tracked in
// daemonSpecs — i.e. the engine still considers this task a live daemon.
// Used by the propagation goroutine to bail out if Unregister has run
// between the time the goroutine was launched and the time it gets
// scheduled (or between successive descendant dispatches in a long
// pipeline). Mirrors the stillRegistered check in onDaemonRunFinished.
func (e *Engine) daemonRegistered(taskID string) bool {
	e.daemonMu.Lock()
	_, ok := e.daemonSpecs[taskID]
	e.daemonMu.Unlock()
	return ok
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
