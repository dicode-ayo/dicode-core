package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// A pipeline stage that calls dicode.suspend() cannot pause the pipeline: the
// parent run is owned by the PipelineRunner and is not resumable through the
// single-run resume path. The engine must therefore (a) finalize the pipeline
// parent to a terminal FAILURE (never leave it `suspended` with no token/deadline
// — a permanently wedged, unsweepable, unresumable parent), and (b) cancel the
// orphaned suspended stage run so it can't be resumed into a continuation
// detached from the pipeline. These tests assert both, for the two consumption
// paths: a non-terminal stage (awaitStageSuccess) and a terminal daemon stage
// (runTerminalDaemon).

// registerStageSpec registers a spec into the registry only. Stages are resolved
// from the registry by firePipeline; engine-registering a daemon stage would
// spawn it standalone, which these tests must avoid.
func registerStageSpec(t *testing.T, reg *registry.Registry, s *task.Spec) {
	t.Helper()
	if err := reg.Register(s); err != nil {
		t.Fatalf("reg.Register %s: %v", s.ID, err)
	}
}

// TestPipelineNonTerminalStageSuspend_FailsParentCancelsStage fires a two-stage
// sequential pipeline whose FIRST (non-terminal) stage suspends. The parent must
// end terminal-failure (not wedged `suspended`), the suspended stage run must be
// cancelled with the pipeline_stage_suspended reason, the downstream stage must
// never run, and nothing must be left resumable.
func TestPipelineNonTerminalStageSuspend_FailsParentCancelsStage(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})

	stageA := &task.Spec{ID: "stage-a", Name: "stage-a", Runtime: task.RuntimeDeno, Enabled: true,
		Trigger: task.TriggerConfig{Manual: true}}
	stageB := &task.Spec{ID: "stage-b", Name: "stage-b", Runtime: task.RuntimeDeno, Enabled: true,
		Trigger: task.TriggerConfig{Manual: true}}
	registerStageSpec(t, reg, stageA)
	registerStageSpec(t, reg, stageB)

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "stage-a"}, {Task: "stage-b"}},
	}
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}

	parentRunID, err := eng.FireManual(context.Background(), "p", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	parent := waitStatus(t, reg, parentRunID, registry.StatusFailure)
	if parent.ResumeToken != "" {
		t.Errorf("pipeline parent carries a resume token %q; must not be resumable", parent.ResumeToken)
	}
	if parent.FinishedAt == nil {
		t.Error("pipeline parent has no finished_at; a failed pipeline must be terminal")
	}

	// The suspended stage run must be finalized to cancelled, not left suspended.
	kids, err := reg.ListChildren(context.Background(), parentRunID, 10)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	var stageARun *registry.Run
	for _, c := range kids {
		if c.TaskID == "stage-a" {
			stageARun = c
		}
		if c.TaskID == "stage-b" {
			t.Errorf("downstream stage-b ran (%s); it must not run after stage-a suspended", c.ID)
		}
	}
	if stageARun == nil {
		t.Fatalf("no stage-a child run recorded; children=%+v", kids)
	}
	if stageARun.Status != registry.StatusCancelled {
		t.Errorf("stage-a status = %q, want cancelled", stageARun.Status)
	}
	if stageARun.FailureReason != reasonPipelineStageSuspended {
		t.Errorf("stage-a fail_reason = %q, want %q", stageARun.FailureReason, reasonPipelineStageSuspended)
	}
	if stageARun.ResumeToken != "" {
		t.Errorf("stage-a still carries a resume token %q; it must be dropped", stageARun.ResumeToken)
	}

	// Nothing may be left in the suspended state — no orphan is resumable.
	if susp, _ := reg.ListSuspendedRuns(context.Background(), 10); len(susp) != 0 {
		t.Errorf("suspended runs remain after pipeline failed: %+v", susp)
	}
}

// TestPipelineTerminalDaemonStageSuspend_FailsParentNotWedged fires a pipeline
// whose single (terminal) daemon stage suspends. This is the path the issue
// flagged as wedging: WaitRun returns `suspended` and finish() previously wrote
// `suspended` onto the parent with no token/deadline. The parent must instead
// end terminal-failure and the stage run must be cancelled.
func TestPipelineTerminalDaemonStageSuspend_FailsParentNotWedged(t *testing.T) {
	eng, reg := newSuspendEnv(t, &suspendExec{})

	daemonStage := &task.Spec{ID: "daemon-stage", Name: "daemon-stage", Runtime: task.RuntimeDeno, Enabled: true,
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"}}
	// Registered in the registry only: engine-registering a daemon would start it
	// standalone, competing with the pipeline for the run.
	registerStageSpec(t, reg, daemonStage)

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "pd", Name: "PD", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "daemon-stage"}},
	}
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}

	parentRunID, err := eng.FireManual(context.Background(), "pd", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	parent := waitStatus(t, reg, parentRunID, registry.StatusFailure)
	if parent.Status == registry.StatusSuspended {
		t.Fatal("pipeline parent left suspended — wedged (unsweepable, unresumable)")
	}
	if parent.ResumeToken != "" {
		t.Errorf("pipeline parent carries a resume token %q; must not be resumable", parent.ResumeToken)
	}
	if parent.ResumeDeadline != 0 {
		t.Errorf("pipeline parent carries a resume deadline %d; must not be set", parent.ResumeDeadline)
	}
	if parent.FinishedAt == nil {
		t.Error("pipeline parent has no finished_at; a failed pipeline must be terminal")
	}
	if parent.FailureReason == "" {
		t.Error("pipeline parent failure carries no reason; a suspended stage must surface a clear cause")
	}

	kids, err := reg.ListChildren(context.Background(), parentRunID, 10)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(kids) != 1 {
		t.Fatalf("want 1 stage child, got %d: %+v", len(kids), kids)
	}
	if kids[0].Status != registry.StatusCancelled {
		t.Errorf("daemon stage status = %q, want cancelled", kids[0].Status)
	}
	if kids[0].FailureReason != reasonPipelineStageSuspended {
		t.Errorf("daemon stage fail_reason = %q, want %q", kids[0].FailureReason, reasonPipelineStageSuspended)
	}

	if susp, _ := reg.ListSuspendedRuns(context.Background(), 10); len(susp) != 0 {
		t.Errorf("suspended runs remain after pipeline failed: %+v", susp)
	}

	// Give the live-pipeline unregister + lifecycle teardown a moment; the test's
	// engine has no shutdown, so ensure no late goroutine re-suspends the parent.
	time.Sleep(50 * time.Millisecond)
	again, _ := reg.GetRun(context.Background(), parentRunID)
	if again.Status != registry.StatusFailure {
		t.Errorf("pipeline parent status drifted to %q after finish; want stable failure", again.Status)
	}
}
