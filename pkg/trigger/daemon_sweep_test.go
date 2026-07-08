package trigger

// Regression tests for issue #502: a standalone daemon that suspends via
// dicode.suspend() and is never resumed used to wedge permanently once its
// resume deadline lapsed. onDaemonRunFinished parked the #470 run slot and set
// DaemonSuspended for a resume that never came; the body goroutine had already
// exited, so when the #509 sweep flipped the run suspended→cancelled it left
// the slot pinned to a now-cancelled run. registerDaemon then read
// alreadyRunning=true and a reconciler reload refused to restart — the restart
// policy bypassed until a full process restart cleared the fresh map.
//
// The fix routes a swept standalone-daemon suspension back through the daemon
// lifecycle (SweepExpiredSuspensions → onDaemonSuspensionSwept): release the
// slot under the same slot-match guard, clear DaemonSuspended, and let the
// restart policy decide.

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

// suspendDaemonEnv wires a suspendExec-backed engine with a standalone daemon
// registered in both the registry and daemonSpecs (so the sweep's daemon-ness
// lookup and onDaemonRunFinished's stillRegistered guard see it), then brings
// the daemon up and waits for it to park in DaemonSuspended. Returns the engine,
// registry, spec, and the suspended run ID holding the parked slot.
func suspendDaemonEnv(t *testing.T, restart string) (*Engine, *registry.Registry, *task.Spec, string) {
	t.Helper()
	exec := &suspendExec{firstDeadline: time.Now().Add(-time.Hour).UnixMilli()}
	eng, reg := newSuspendEnv(t, exec)

	spec := &task.Spec{
		ID:      "d-suspend",
		Name:    "d-suspend",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Daemon: true, Restart: restart},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	eng.daemonMu.Lock()
	eng.daemonSpecs[spec.ID] = spec
	eng.daemonMu.Unlock()

	eng.startDaemon(spec)

	var suspendedRunID string
	waitUntil(t, 5*time.Second, func() bool {
		eng.daemonMu.Lock()
		suspendedRunID = eng.daemonRuns[spec.ID]
		eng.daemonMu.Unlock()
		return suspendedRunID != "" && eng.DaemonState(spec.ID) == DaemonSuspended
	}, "daemon never parked in DaemonSuspended with a reserved slot")
	return eng, reg, spec, suspendedRunID
}

func daemonSlot(eng *Engine, id string) string {
	eng.daemonMu.Lock()
	defer eng.daemonMu.Unlock()
	return eng.daemonRuns[id]
}

// TestSweep_SuspendedDaemonTimeout_ReleasesSlotAndRestarts is the #502 wedge
// regression: a restart:always standalone daemon that suspended and timed out
// must have its slot released and restart per policy — not stay pinned to the
// cancelled run.
func TestSweep_SuspendedDaemonTimeout_ReleasesSlotAndRestarts(t *testing.T) {
	eng, reg, spec, suspendedRunID := suspendDaemonEnv(t, "always")

	swept, err := eng.SweepExpiredSuspensions(context.Background(), time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("SweepExpiredSuspensions: %v", err)
	}
	if len(swept) != 1 || swept[0] != suspendedRunID {
		t.Fatalf("swept = %v, want [%s]", swept, suspendedRunID)
	}

	// The restart fires a fresh body (which suspends again), so the slot must end
	// up parked on a DIFFERENT run than the swept, now-cancelled one.
	waitUntil(t, 5*time.Second, func() bool {
		slot := daemonSlot(eng, spec.ID)
		return slot != "" && slot != suspendedRunID
	}, "daemon slot never re-parked on a restarted body — it wedged on the cancelled run")

	// A second run exists for the daemon: proof the restart actually ran a body,
	// not just cleared the slot.
	waitRunCount(t, reg, spec.ID, 2)

	// The swept run is terminal cancelled with the resume-timeout reason.
	run, _ := reg.GetRun(context.Background(), suspendedRunID)
	if run.Status != registry.StatusCancelled || run.FailureReason != registry.ReasonResumeTimeout {
		t.Errorf("swept run status/reason = %q/%q, want cancelled/resume_timeout", run.Status, run.FailureReason)
	}
}

// TestSweep_SuspendedDaemonTimeout_RestartNever_NoRestart confirms the sweep
// honors the restart policy: a restart:never daemon whose suspension timed out
// releases its slot (so it is no longer wedged) but is NOT restarted, and lands
// in the terminal DaemonCrashed state rather than a stale DaemonSuspended.
func TestSweep_SuspendedDaemonTimeout_RestartNever_NoRestart(t *testing.T) {
	eng, reg, spec, suspendedRunID := suspendDaemonEnv(t, "never")

	if _, err := eng.SweepExpiredSuspensions(context.Background(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("SweepExpiredSuspensions: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		return daemonSlot(eng, spec.ID) == "" && eng.DaemonState(spec.ID) == DaemonCrashed
	}, "restart:never daemon did not release its slot / reach DaemonCrashed")

	// No restart: only the original suspended body ever ran.
	time.Sleep(50 * time.Millisecond)
	runs, err := reg.ListRuns(context.Background(), spec.ID, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != suspendedRunID {
		t.Fatalf("restart:never spawned a new run: got %d runs %v, want only the swept one", len(runs), runs)
	}
}

// TestOnDaemonSuspensionSwept_ConcurrentResumeSlotNotClobbered pins the
// slot-match guard: if a resume continuation has already adopted the slot
// (daemonRuns points at a different run than the swept one), the sweep's slot
// handling must leave it alone rather than delete the continuation's slot.
func TestOnDaemonSuspensionSwept_ConcurrentResumeSlotNotClobbered(t *testing.T) {
	eng, reg, spec, suspendedRunID := suspendDaemonEnv(t, "always")

	run, err := reg.GetRun(context.Background(), suspendedRunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	// Model a resume continuation that adopted the slot before the sweep's
	// out-of-band slot handling ran.
	const continuationRunID = "continuation-run"
	eng.daemonMu.Lock()
	eng.daemonRuns[spec.ID] = continuationRunID
	eng.daemonMu.Unlock()

	// Drive the sweep's daemon handler directly with the swept (old) run.
	eng.onDaemonSuspensionSwept(spec, run)

	if slot := daemonSlot(eng, spec.ID); slot != continuationRunID {
		t.Fatalf("daemon slot = %q, want the continuation's %q left untouched", slot, continuationRunID)
	}
}

// TestSweep_SuspendedPipelineStageDaemon_DoesNotTouchDaemonRuns confirms a
// pipeline-stage daemon run (TriggerPipelineStage) that suspends and is swept is
// NOT treated as a standalone daemon: the standalone daemon's parked slot for
// the SAME task must be left untouched, since a pipeline stage is owned by the
// PipelineRunner, not the standalone-daemon machinery.
func TestSweep_SuspendedPipelineStageDaemon_DoesNotTouchDaemonRuns(t *testing.T) {
	exec := &suspendExec{firstDeadline: time.Now().Add(-time.Hour).UnixMilli()}
	eng, reg := newSuspendEnv(t, exec)

	spec := &task.Spec{
		ID:      "d-both",
		Name:    "d-both",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Daemon: true, Restart: "always"},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	// A live standalone daemon body of this task holds the slot.
	const standaloneRunID = "standalone-body"
	eng.daemonMu.Lock()
	eng.daemonSpecs[spec.ID] = spec
	eng.daemonRuns[spec.ID] = standaloneRunID
	eng.daemonMu.Unlock()

	// Fire the SAME task as a pipeline stage; it suspends with a past deadline.
	// The run.go finish gate skips onDaemonRunFinished for pipeline-stage runs,
	// so the standalone slot is untouched by the suspend.
	stageRunID, err := eng.fireAsync(context.Background(), spec, pkgruntime.RunOptions{}, registry.TriggerPipelineStage)
	if err != nil {
		t.Fatalf("fireAsync pipeline stage: %v", err)
	}
	waitStatus(t, reg, stageRunID, registry.StatusSuspended)

	swept, err := eng.SweepExpiredSuspensions(context.Background(), time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("SweepExpiredSuspensions: %v", err)
	}
	if len(swept) != 1 || swept[0] != stageRunID {
		t.Fatalf("swept = %v, want [%s]", swept, stageRunID)
	}

	// The standalone daemon's slot must be exactly as it was — the pipeline-stage
	// sweep must not release or restart it.
	time.Sleep(50 * time.Millisecond)
	if slot := daemonSlot(eng, spec.ID); slot != standaloneRunID {
		t.Fatalf("standalone daemon slot = %q, want %q untouched by pipeline-stage sweep", slot, standaloneRunID)
	}
}
