package trigger

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// finishHookRecorder captures every runFinishedHook invocation.
type finishHookRecorder struct {
	mu     sync.Mutex
	events []finishEvent
}

type finishEvent struct {
	taskID, runID, status, triggerSource string
}

func (r *finishHookRecorder) hook(taskID, runID, status, triggerSource string, _ int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, finishEvent{taskID, runID, status, triggerSource})
}

// forRunStatus returns the captured events for runID with the given status.
// A suspend also fires the finished hook (status=suspended), so callers filter
// to the terminal status they assert on.
func (r *finishHookRecorder) forRunStatus(runID, status string) []finishEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []finishEvent
	for _, e := range r.events {
		if e.runID == runID && e.status == status {
			out = append(out, e)
		}
	}
	return out
}

// waitRunCount polls until task taskID has at least want runs, or fails.
func waitRunCount(t *testing.T, reg *registry.Registry, taskID string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := reg.ListRuns(context.Background(), taskID, 10)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(runs) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	runs, _ := reg.ListRuns(context.Background(), taskID, 10)
	t.Fatalf("task %s run count = %d, want >= %d", taskID, len(runs), want)
}

// A suspended run swept past its deadline fires the run:finished hook with the
// resume_timeout cancellation and drives an on:always chain — the same finish
// side-effects a normal cancellation performs.
func TestEngineSweep_FiresFinishHookAndResumeTimeoutChain(t *testing.T) {
	exec := &suspendExec{firstDeadline: time.Now().Add(-time.Hour).UnixMilli()}
	eng, reg := newSuspendEnv(t, exec)

	rec := &finishHookRecorder{}
	eng.AddRunFinishedHook(rec.hook)

	wiz := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	after := &task.Spec{ID: "after", Name: "after", Runtime: task.RuntimeDeno, Enabled: true,
		Trigger: task.TriggerConfig{Chain: &task.ChainTrigger{From: "wiz", On: chainOnAlways}}}
	if err := reg.Register(wiz); err != nil {
		t.Fatalf("register wiz: %v", err)
	}
	if err := reg.Register(after); err != nil {
		t.Fatalf("register after: %v", err)
	}
	eng.Register(after) // arm the chain

	origID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitStatus(t, reg, origID, registry.StatusSuspended)

	swept, err := eng.SweepExpiredSuspensions(context.Background(), time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("SweepExpiredSuspensions: %v", err)
	}
	if len(swept) != 1 || swept[0] != origID {
		t.Fatalf("swept = %v, want [%s]", swept, origID)
	}

	// The registry row reflects the timeout cancellation.
	run, _ := reg.GetRun(context.Background(), origID)
	if run.Status != registry.StatusCancelled || run.FailureReason != registry.ReasonResumeTimeout {
		t.Errorf("swept run status/reason = %q/%q, want cancelled/resume_timeout", run.Status, run.FailureReason)
	}

	// The run:finished hook fired exactly once for the swept run, as cancelled.
	events := rec.forRunStatus(origID, registry.StatusCancelled)
	if len(events) != 1 {
		t.Fatalf("cancelled finish hook fired %d times for swept run, want 1: %+v", len(events), events)
	}
	if events[0].taskID != "wiz" {
		t.Errorf("finish event = %+v, want task wiz", events[0])
	}

	// The on:always chain observed the resume_timeout cancellation and fired.
	waitRunCount(t, reg, "after", 1)
}

// A run that resumed just before the sweep must not be finished a second time:
// the sweep skips any candidate its status-guarded UPDATE didn't transition, so
// no run:finished hook fires for it and its terminal state is preserved.
func TestEngineSweep_ResumedRunNotDoubleFinished(t *testing.T) {
	exec := &suspendExec{firstDeadline: time.Now().Add(-time.Hour).UnixMilli()}
	eng, reg := newSuspendEnv(t, exec)

	rec := &finishHookRecorder{}
	eng.AddRunFinishedHook(rec.hook)

	wiz := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(wiz); err != nil {
		t.Fatalf("register wiz: %v", err)
	}

	origID, err := eng.FireManual(context.Background(), "wiz", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	// The run resumes (token consumed → terminal `resumed`) just before the sweep.
	if err := reg.MarkRunResumed(context.Background(), orig.ID); err != nil {
		t.Fatalf("MarkRunResumed: %v", err)
	}

	swept, err := eng.SweepExpiredSuspensions(context.Background(), time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("SweepExpiredSuspensions: %v", err)
	}
	if len(swept) != 0 {
		t.Fatalf("swept = %v, want empty (run already resumed)", swept)
	}
	if events := rec.forRunStatus(origID, registry.StatusCancelled); len(events) != 0 {
		t.Fatalf("cancelled finish hook fired for an already-resumed run: %+v", events)
	}

	// The resume outcome is preserved — the sweep did not overwrite it.
	run, _ := reg.GetRun(context.Background(), origID)
	if run.Status != registry.StatusResumed {
		t.Errorf("run status = %q, want resumed (sweep must not clobber it)", run.Status)
	}
}
