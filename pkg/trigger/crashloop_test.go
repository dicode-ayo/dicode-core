package trigger

// Crash-loop detection tests (issue #458).
//
// Two layers of coverage:
//
//   - crashloopTracker unit tests: the counting rule in isolation (threshold,
//     N-1 boundary, sustained/clean-exit resets, lazy in-flight recovery);
//   - engine-level tests: onDaemonRunFinished feeds the tracker, and the
//     status-derivation choke point (Engine.DaemonState / IsCrashLooping)
//     reports "crashlooping" even while a respawn is momentarily in flight
//     with daemonStates saying Running — the exact masking window from the
//     issue report.

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// quickFail is any run lifetime comfortably below crashloopSustainWindow.
const quickFail = 500 * time.Millisecond

func TestCrashloopTracker_ThresholdConsecutiveQuickFailures(t *testing.T) {
	tr := newCrashloopTracker()

	// N-1 quick failures must NOT trip the detector.
	for i := 0; i < crashloopThreshold-1; i++ {
		tr.noteExit("d", quickFail, false)
		if tr.isCrashLooping("d") {
			t.Fatalf("crashlooping after %d quick failures, want threshold %d", i+1, crashloopThreshold)
		}
	}
	// The Nth consecutive quick failure trips it.
	if got := tr.noteExit("d", quickFail, false); got != crashloopThreshold {
		t.Fatalf("noteExit count = %d, want %d", got, crashloopThreshold)
	}
	if !tr.isCrashLooping("d") {
		t.Fatalf("not crashlooping after %d consecutive quick failures", crashloopThreshold)
	}
	// Other tasks are unaffected.
	if tr.isCrashLooping("other") {
		t.Error("unrelated task reports crashlooping")
	}
}

func TestCrashloopTracker_SustainedExitResets(t *testing.T) {
	tr := newCrashloopTracker()
	for i := 0; i < crashloopThreshold; i++ {
		tr.noteExit("d", quickFail, false)
	}
	if !tr.isCrashLooping("d") {
		t.Fatal("precondition: expected crashlooping")
	}
	// A failure exit that outlasted the sustain window resets the counter —
	// the daemon came up properly; this is a fresh crash, not part of a loop.
	if got := tr.noteExit("d", crashloopSustainWindow, false); got != 0 {
		t.Fatalf("sustained exit: count = %d, want 0", got)
	}
	if tr.isCrashLooping("d") {
		t.Error("still crashlooping after a sustained run")
	}
	// And the next quick failure starts counting from 1 again.
	if got := tr.noteExit("d", quickFail, false); got != 1 {
		t.Errorf("count after reset = %d, want 1", got)
	}
}

func TestCrashloopTracker_CleanExitResets(t *testing.T) {
	tr := newCrashloopTracker()
	for i := 0; i < crashloopThreshold; i++ {
		tr.noteExit("d", quickFail, false)
	}
	// A success exit resets even when it was quick.
	if got := tr.noteExit("d", quickFail, true); got != 0 {
		t.Fatalf("clean exit: count = %d, want 0", got)
	}
	if tr.isCrashLooping("d") {
		t.Error("still crashlooping after a clean exit")
	}
}

func TestCrashloopTracker_ResetClears(t *testing.T) {
	tr := newCrashloopTracker()
	for i := 0; i < crashloopThreshold; i++ {
		tr.noteExit("d", quickFail, false)
	}
	tr.reset("d") // operator cancel / unregister path
	if tr.isCrashLooping("d") {
		t.Error("still crashlooping after reset")
	}
	if got := tr.noteExit("d", quickFail, false); got != 1 {
		t.Errorf("count after reset = %d, want 1", got)
	}
}

// TestCrashloopTracker_InFlightSustainedSpawnRecovers pins the lazy recovery
// rule: once a live respawn has survived crashloopSustainWindow, the daemon
// has recovered and isCrashLooping self-clears — without waiting for the run
// to exit (a healthy daemon may run for days).
func TestCrashloopTracker_InFlightSustainedSpawnRecovers(t *testing.T) {
	now := time.Now()
	tr := newCrashloopTracker()
	tr.now = func() time.Time { return now }

	for i := 0; i < crashloopThreshold; i++ {
		tr.noteExit("d", quickFail, false)
	}
	tr.noteSpawn("d")

	// While the spawn is younger than the window, still crashlooping — this
	// is exactly the spawn-before-crash window that must not read healthy.
	now = now.Add(crashloopSustainWindow / 2)
	if !tr.isCrashLooping("d") {
		t.Fatal("young in-flight spawn cleared crashlooping prematurely")
	}

	// Once the spawn has sustained the window, the daemon has recovered.
	now = now.Add(crashloopSustainWindow)
	if tr.isCrashLooping("d") {
		t.Fatal("sustained in-flight spawn did not clear crashlooping")
	}
	// The reset is sticky: a later quick failure counts from 1.
	if got := tr.noteExit("d", quickFail, false); got != 1 {
		t.Errorf("count after lazy recovery = %d, want 1", got)
	}
}

// crashloopEnv is finishedRunEnv's sibling for the crash-loop tests: a test
// engine with a registered restart=never daemon spec (so onDaemonRunFinished
// returns without sleeping through the restart backoff), daemonStates
// pre-seeded to Running, and the raw db handle returned so tests can backdate
// run rows.
func crashloopEnv(t *testing.T) (*Engine, *task.Spec, db.DB) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	exec := newPreflightExec(reg)
	eng := New(reg, exec, zap.NewNop())
	eng.RegisterExecutor(task.RuntimeDocker, exec)

	spec := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	// Mark the daemon as still-registered with the engine so the
	// onDaemonRunFinished early-return guard doesn't bail out.
	eng.daemonMu.Lock()
	eng.daemonSpecs[spec.ID] = spec
	eng.daemonMu.Unlock()
	eng.setDaemonState(spec.ID, DaemonRunning)
	return eng, spec, d
}

// quickFailureRun seeds a run row that started recently and finished with the
// given status — i.e. a quick exit (< crashloopSustainWindow) as seen by
// onDaemonRunFinished's elapsed computation.
func quickFailureRun(t *testing.T, eng *Engine, taskID, status string) string {
	t.Helper()
	ctx := context.Background()
	runID, err := eng.registry.StartRun(ctx, taskID, "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := eng.registry.FinishRun(ctx, runID, status); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return runID
}

// TestOnDaemonRunFinished_QuickFailures_ReportCrashLooping drives the real
// engine hook with consecutive quick failures (restart=never so the hook
// returns without sleeping through the restart backoff) and asserts the
// status-derivation choke point flips to crashlooping at the threshold —
// and, crucially, keeps reporting crashlooping even while a respawn is
// momentarily in flight (daemonStates says Running).
func TestOnDaemonRunFinished_QuickFailures_ReportCrashLooping(t *testing.T) {
	eng, spec, _ := crashloopEnv(t)

	for i := 0; i < crashloopThreshold-1; i++ {
		runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)
		eng.onDaemonRunFinished(spec, runID)
		if eng.IsCrashLooping(spec.ID) {
			t.Fatalf("crashlooping after %d quick failures, want threshold %d", i+1, crashloopThreshold)
		}
	}
	// Boundary at N-1: still the plain no-restart state.
	if got := eng.DaemonState(spec.ID); got != DaemonCrashed {
		t.Fatalf("DaemonState at N-1 failures = %q, want %q", got, DaemonCrashed)
	}

	// Nth quick failure → crashlooping.
	runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)
	eng.onDaemonRunFinished(spec, runID)
	if !eng.IsCrashLooping(spec.ID) {
		t.Fatalf("not crashlooping after %d consecutive quick failures", crashloopThreshold)
	}
	if got := eng.DaemonState(spec.ID); got != DaemonCrashLooping {
		t.Fatalf("DaemonState = %q, want %q", got, DaemonCrashLooping)
	}

	// The masking window from issue #458: a respawn is momentarily in flight,
	// so the underlying lifecycle map says Running — the derived status must
	// still be crashlooping, never the transient running.
	eng.crashloops.noteSpawn(spec.ID)
	eng.setDaemonState(spec.ID, DaemonRunning)
	if got := eng.DaemonState(spec.ID); got != DaemonCrashLooping {
		t.Fatalf("DaemonState during transient spawn = %q, want %q (running must never mask a crash loop)",
			got, DaemonCrashLooping)
	}
	if !eng.IsCrashLooping(spec.ID) {
		t.Fatal("IsCrashLooping = false during transient spawn window")
	}
}

// TestOnDaemonRunFinished_CleanExitResetsCrashLoop: a success exit ends the
// loop state and normal status derivation resumes.
func TestOnDaemonRunFinished_CleanExitResetsCrashLoop(t *testing.T) {
	eng, spec, _ := crashloopEnv(t)

	for i := 0; i < crashloopThreshold; i++ {
		runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)
		eng.onDaemonRunFinished(spec, runID)
	}
	if !eng.IsCrashLooping(spec.ID) {
		t.Fatal("precondition: expected crashlooping")
	}

	runID := quickFailureRun(t, eng, spec.ID, registry.StatusSuccess)
	eng.onDaemonRunFinished(spec, runID)
	if eng.IsCrashLooping(spec.ID) {
		t.Fatal("still crashlooping after a clean exit")
	}
	if got := eng.DaemonState(spec.ID); got != DaemonStopped {
		t.Errorf("DaemonState after clean exit = %q, want %q", got, DaemonStopped)
	}
}

// TestOnDaemonRunFinished_SustainedRunResetsCrashLoop: an exit whose run
// outlasted the sustain window resets the counter even when it failed.
func TestOnDaemonRunFinished_SustainedRunResetsCrashLoop(t *testing.T) {
	eng, spec, d := crashloopEnv(t)

	for i := 0; i < crashloopThreshold; i++ {
		runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)
		eng.onDaemonRunFinished(spec, runID)
	}
	if !eng.IsCrashLooping(spec.ID) {
		t.Fatal("precondition: expected crashlooping")
	}

	// Seed a failed run that lasted past the sustain window by backdating
	// its started_at (stored as unix milliseconds).
	ctx := context.Background()
	runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)
	backdated := time.Now().Add(-2 * crashloopSustainWindow).UnixMilli()
	if err := d.Exec(ctx,
		`UPDATE runs SET started_at = ? WHERE id = ?`, backdated, runID,
	); err != nil {
		t.Fatalf("backdate started_at: %v", err)
	}
	eng.onDaemonRunFinished(spec, runID)
	if eng.IsCrashLooping(spec.ID) {
		t.Fatal("still crashlooping after a sustained run")
	}
}

// TestOnDaemonRunFinished_CancelledResetsCrashLoop: an operator kill is
// deliberate intent — the stopped daemon must not keep reporting the loop.
func TestOnDaemonRunFinished_CancelledResetsCrashLoop(t *testing.T) {
	eng, spec, _ := crashloopEnv(t)

	for i := 0; i < crashloopThreshold; i++ {
		runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)
		eng.onDaemonRunFinished(spec, runID)
	}
	if !eng.IsCrashLooping(spec.ID) {
		t.Fatal("precondition: expected crashlooping")
	}

	runID := quickFailureRun(t, eng, spec.ID, registry.StatusCancelled)
	eng.onDaemonRunFinished(spec, runID)
	if eng.IsCrashLooping(spec.ID) {
		t.Fatal("still crashlooping after operator cancellation")
	}
	if got := eng.DaemonState(spec.ID); got != DaemonStopped {
		t.Errorf("DaemonState after cancellation = %q, want %q", got, DaemonStopped)
	}
}

// TestUnregister_ResetsCrashLoop: unregistration wipes the tracker so a
// removed/reloaded task starts with a fresh counter.
func TestUnregister_ResetsCrashLoop(t *testing.T) {
	eng, spec, _ := crashloopEnv(t)

	for i := 0; i < crashloopThreshold; i++ {
		runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)
		eng.onDaemonRunFinished(spec, runID)
	}
	if !eng.IsCrashLooping(spec.ID) {
		t.Fatal("precondition: expected crashlooping")
	}

	eng.Unregister(spec.ID)
	if eng.IsCrashLooping(spec.ID) {
		t.Fatal("still crashlooping after Unregister")
	}
}
