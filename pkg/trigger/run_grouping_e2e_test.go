package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestEngine_RunTask_ParentLinkage_E2E covers the third dispatch source from
// issue #116: when a running task calls dicode.run_task(child), the new run
// must have parent_run_id pointing back at the caller.
//
// This is a true end-to-end integration test — real Deno subprocess, real
// IPC server, real registry, real engine. It complements the unit-level
// IPC test (TestServer_Dicode_RunTask_ParentLinkage) which uses a mock
// EngineRunner and only verifies the IPC handler forwards s.runID; this
// test verifies fireAsync actually persists the parent ID end-to-end.
func TestEngine_RunTask_ParentLinkage_E2E(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	// Wire the engine into the Deno runtime so dicode.run_task works.
	e.denoRT.SetEngine(e.engine)

	// Child task: trivial, returns its own run id so the parent can record it.
	child := writeTask(t, dir, "rg-child",
		`export default async function main({ dicode }) {
			await dicode.set_group("conversation-7")
			return dicode.run_id
		}`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(child)
	e.engine.Register(child)

	// Parent task: uses dicode.run_task to fire the child synchronously,
	// then returns the child's run id from the call result.
	parent := writeTask(t, dir, "rg-parent",
		`export default async function main({ dicode }) {
			const result = await dicode.run_task("rg-child")
			return result?.runID ?? null
		}`,
		task.TriggerConfig{Manual: true})
	parent.Permissions.Dicode = &task.DicodePermissions{Tasks: []string{"rg-child"}}
	_ = e.reg.Register(parent)
	e.engine.Register(parent)

	parentRunID, err := e.engine.FireManual(context.Background(), "rg-parent", nil)
	if err != nil {
		t.Fatalf("FireManual parent: %v", err)
	}

	// Wait for the parent to finish (this implicitly waits for the child too,
	// since the parent blocks on dicode.run_task → WaitRun).
	parentRun := waitForRun(t, e.reg, parentRunID, 60*time.Second)
	if parentRun.Status != registry.StatusSuccess {
		t.Fatalf("parent status = %s, want success", parentRun.Status)
	}

	// The parent's return_value is the child's run ID (JSON-encoded string).
	// Strip the quotes — JSON-encoded "abc-123" is `"abc-123"` on disk.
	if len(parentRun.ReturnValue) < 2 {
		t.Fatalf("parent return_value unexpectedly empty: %q", parentRun.ReturnValue)
	}
	childRunID := parentRun.ReturnValue[1 : len(parentRun.ReturnValue)-1]

	childRun, err := e.reg.GetRun(context.Background(), childRunID)
	if err != nil {
		t.Fatalf("GetRun child: %v", err)
	}
	if childRun.ParentRunID != parentRunID {
		t.Errorf("child.ParentRunID = %q, want %q (the parent run)", childRun.ParentRunID, parentRunID)
	}
	// set_group landed on the child too — proves the SDK call wired through
	// IPC, lining up the second half of #116 in the same flow.
	if childRun.Group != "conversation-7" {
		t.Errorf("child.Group = %q, want conversation-7", childRun.Group)
	}
}

// waitForRun polls the registry until the run reaches a terminal state or
// the deadline expires, then returns the final record. Fails the test if
// the run is still running at the deadline.
func waitForRun(t *testing.T, reg *registry.Registry, runID string, timeout time.Duration) *registry.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run, err := reg.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun %s: %v", runID, err)
		}
		if run.Status != registry.StatusRunning {
			return run
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish within %s", runID, timeout)
	return nil
}
