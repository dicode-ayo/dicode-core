package trigger

import "sync"

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
