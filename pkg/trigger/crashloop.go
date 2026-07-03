package trigger

// Crash-loop detection for daemon tasks (issue #458).
//
// A daemon that crash-loops (spawn → crash within seconds → backoff →
// respawn) intermittently shows a transient "running" run as its latest run,
// because status sampling can land in the brief spawn-before-crash window.
// The crashloopTracker counts consecutive quick failures per daemon so status
// consumers (CLI list/status, WebUI, REST API) can surface a distinct
// "crashlooping" state instead of a misleading "running".
//
// Detection rule:
//   - a "quick failure" is a daemon-body exit with a non-success, non-cancelled
//     status whose run lasted less than crashloopSustainWindow;
//   - after crashloopThreshold consecutive quick failures the daemon is
//     considered crash-looping;
//   - the counter resets on any clean (success) exit, on any exit that
//     outlasted the sustain window, on operator cancellation, on task
//     unregistration, and — lazily — once an in-flight spawn has survived
//     the sustain window (so a daemon that finally sustains stops reporting
//     "crashlooping" without waiting for its next exit).

import (
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/ipc"
)

// The control server surfaces crash-loop state via an optional type
// assertion on its EngineRunner; pin the contract at compile time so a
// signature drift can't silently disable the cli.list / cli.status override.
var _ ipc.CrashloopReporter = (*Engine)(nil)

const (
	// crashloopThreshold is the number of consecutive quick failures after
	// which a daemon task is reported as crash-looping.
	crashloopThreshold = 3

	// crashloopSustainWindow is how long a daemon run must survive for its
	// start to count as sustained (resetting the quick-failure counter).
	// Deliberately the same value as the restart-backoff's stable threshold:
	// both answer "did this daemon actually come up?".
	crashloopSustainWindow = daemonStableThreshold
)

// crashloopEntry is the per-daemon tracking record.
type crashloopEntry struct {
	// quickFails is the count of consecutive daemon-body exits that were
	// quick failures (see package comment for the exact rule).
	quickFails int
	// spawnedAt is the time of the most recent in-flight spawn, or the zero
	// time when no spawn is live. Used only for the lazy "the current run
	// has sustained" recovery check.
	spawnedAt time.Time
}

// crashloopTracker is a thread-safe taskID → crashloopEntry map. Purely
// in-memory: crash-loop state is daemon-lifecycle state, not history, and a
// daemon restart starts every counter fresh (matching daemonBackoffs).
type crashloopTracker struct {
	mu      sync.Mutex
	entries map[string]*crashloopEntry
	now     func() time.Time // injectable for tests; defaults to time.Now
}

func newCrashloopTracker() *crashloopTracker {
	return &crashloopTracker{
		entries: make(map[string]*crashloopEntry),
		now:     time.Now,
	}
}

// noteSpawn records that a daemon body was just (re)started. Only tracked for
// daemons that already have quick failures on record — healthy daemons never
// allocate an entry, keeping the map footprint proportional to the number of
// currently-unhealthy daemons.
func (t *crashloopTracker) noteSpawn(taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[taskID]; ok {
		e.spawnedAt = t.now()
	}
}

// noteExit records a daemon-body exit and returns the updated consecutive
// quick-failure count. clean marks a success exit; elapsed is the run's
// lifetime (FinishedAt − StartedAt; callers pass 0 when FinishedAt is
// missing, which pessimistically counts as an instant crash — matching the
// restart-backoff's treatment of abnormal exits).
//
// Operator cancellations must NOT be routed here — call reset instead: a
// killed daemon is deliberate operator intent, not a crash.
func (t *crashloopTracker) noteExit(taskID string, elapsed time.Duration, clean bool) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if clean || elapsed >= crashloopSustainWindow {
		delete(t.entries, taskID)
		return 0
	}
	e, ok := t.entries[taskID]
	if !ok {
		e = &crashloopEntry{}
		t.entries[taskID] = e
	}
	e.quickFails++
	e.spawnedAt = time.Time{} // no live spawn anymore
	return e.quickFails
}

// clearSpawn drops the live-spawn timestamp without touching the failure
// count. Called when a spawn recorded via noteSpawn failed to launch (no run
// is live), so the timestamp cannot age into a fake "sustained run".
func (t *crashloopTracker) clearSpawn(taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[taskID]; ok {
		e.spawnedAt = time.Time{}
	}
}

// reset clears all crash-loop state for a task. Called on operator
// cancellation and on unregistration.
func (t *crashloopTracker) reset(taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, taskID)
}

// isCrashLooping reports whether the daemon has crossed crashloopThreshold
// consecutive quick failures. If an in-flight spawn has already survived the
// sustain window, the daemon has recovered: the counter is reset eagerly and
// false is returned, so a finally-sustaining daemon stops reporting
// "crashlooping" without waiting for its next exit.
func (t *crashloopTracker) isCrashLooping(taskID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[taskID]
	if !ok || e.quickFails < crashloopThreshold {
		return false
	}
	if !e.spawnedAt.IsZero() && t.now().Sub(e.spawnedAt) >= crashloopSustainWindow {
		delete(t.entries, taskID)
		return false
	}
	return true
}

// IsCrashLooping reports whether the daemon task is currently crash-looping —
// i.e. its body has failed crashloopThreshold consecutive starts, each dying
// within crashloopSustainWindow, and no in-flight run has sustained yet.
//
// This is the single source of truth for every status consumer (CLI
// list/status via pkg/ipc, the WebUI task list, and the REST API's
// daemon_state): while it returns true, the task's displayed status must be
// "crashlooping" — never the transient "running" of a spawn that is about to
// die. Safe for concurrent use. Always false for non-daemon / unknown IDs.
func (e *Engine) IsCrashLooping(taskID string) bool {
	return e.crashloops.isCrashLooping(taskID)
}
