package trigger

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// waitForRunOfTask polls the registry's run list for a run of the given
// task ID that reached a terminal state. Used to observe secondary runs
// fired by the chain-dispatch path.
func waitForRunOfTask(t *testing.T, e *Engine, taskID string, timeout time.Duration) *registry.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		runs, err := e.registry.ListRuns(context.Background(), taskID, 5)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, r := range runs {
			if r.Status == registry.StatusSuccess || r.Status == registry.StatusFailure {
				return r
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// ptrSpec is a helper because task.Spec.OnFailureChain is *task.OnFailureChainSpec so
// {task: ""} disables the default and nil uses the default.
func ptrSpec(taskID string) *task.OnFailureChainSpec {
	return &task.OnFailureChainSpec{Task: taskID}
}

// TestEngine_SetDefaultsOnFailureChain_FiresFallbackOnFailure verifies that
// after SetDefaultsOnFailureChain("fallback-task"), a failing run of any
// other task dispatches the fallback.
func TestEngine_SetDefaultsOnFailureChain_FiresFallbackOnFailure(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	fallback := writeTask(t, dir, "fallback-task",
		`export default async function main() { return "fallback-ran" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(fallback)

	failing := writeTask(t, dir, "failing-task",
		`export default async function main() { throw new Error("boom") }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(failing)

	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{Task: "fallback-task"}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

	runID, err := e.engine.FireManual(context.Background(), "failing-task", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	failed := waitForTerminal(t, e.engine, runID, 30*time.Second)
	if failed.Status != registry.StatusFailure {
		t.Fatalf("primary run status = %q, want failure", failed.Status)
	}

	got := waitForRunOfTask(t, e.engine, "fallback-task", 15*time.Second)
	if got == nil {
		t.Fatal("fallback task never ran after primary failure")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("fallback status = %q, want success", got.Status)
	}
}

// TestEngine_SetDefaultsOnFailureChain_NotFiredOnSuccess verifies the
// default fallback does NOT run when the primary task succeeds.
func TestEngine_SetDefaultsOnFailureChain_NotFiredOnSuccess(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	fallback := writeTask(t, dir, "noop-fallback",
		`export default async function main() { return "fallback-ran" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(fallback)

	ok := writeTask(t, dir, "ok-task",
		`export default async function main() { return "ok" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(ok)

	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{Task: "noop-fallback"}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

	runID, err := e.engine.FireManual(context.Background(), "ok-task", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitForTerminal(t, e.engine, runID, 30*time.Second)
	if run.Status != registry.StatusSuccess {
		t.Fatalf("primary status = %q, want success", run.Status)
	}

	// Give the chain dispatcher a window to (incorrectly) fire. 2s is
	// generous — chain dispatch is synchronous-in-goroutine post-completion.
	time.Sleep(2 * time.Second)
	runs, err := e.engine.registry.ListRuns(context.Background(), "noop-fallback", 5)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) > 0 {
		t.Errorf("noop-fallback should not have run on primary success, got %d runs", len(runs))
	}
}

// TestEngine_OnFailureChain_PerTaskOverride verifies that a task with its
// own `on_failure_chain` overrides the engine-level default.
func TestEngine_OnFailureChain_PerTaskOverride(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	globalFallback := writeTask(t, dir, "global-fallback",
		`export default async function main() { return "should-not-run" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(globalFallback)

	taskFallback := writeTask(t, dir, "task-fallback",
		`export default async function main() { return "task-override-ran" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(taskFallback)

	failing := writeTask(t, dir, "failing-with-override",
		`export default async function main() { throw new Error("boom") }`,
		task.TriggerConfig{Manual: true})
	failing.OnFailureChain = ptrSpec("task-fallback")
	_ = e.reg.Register(failing)

	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{Task: "global-fallback"}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

	runID, err := e.engine.FireManual(context.Background(), "failing-with-override", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	// Per-task override should have run.
	got := waitForRunOfTask(t, e.engine, "task-fallback", 15*time.Second)
	if got == nil {
		t.Fatal("task-level fallback never ran")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("task-level fallback status = %q, want success", got.Status)
	}

	// Global fallback should NOT have run.
	time.Sleep(1 * time.Second) // give stragglers time to land
	globals, _ := e.engine.registry.ListRuns(context.Background(), "global-fallback", 5)
	if len(globals) > 0 {
		t.Errorf("global-fallback should not have run when per-task override is set; got %d runs", len(globals))
	}
}

// TestEngine_OnFailureChain_EmptyStringDisablesDefault verifies that a
// task with OnFailureChain = "" (pointer to empty string) opts OUT of the
// engine-level default even when one is configured.
func TestEngine_OnFailureChain_EmptyStringDisablesDefault(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	globalFallback := writeTask(t, dir, "global-fallback-disabled",
		`export default async function main() { return "should-not-run" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(globalFallback)

	failing := writeTask(t, dir, "opt-out-task",
		`export default async function main() { throw new Error("boom") }`,
		task.TriggerConfig{Manual: true})
	failing.OnFailureChain = ptrSpec("")
	_ = e.reg.Register(failing)

	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{Task: "global-fallback-disabled"}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

	runID, err := e.engine.FireManual(context.Background(), "opt-out-task", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	time.Sleep(2 * time.Second)
	runs, _ := e.engine.registry.ListRuns(context.Background(), "global-fallback-disabled", 5)
	if len(runs) > 0 {
		t.Errorf("empty OnFailureChain should disable default, got %d runs", len(runs))
	}
}

// TestEngine_OnFailureChain_NoLoopSelfReference verifies the engine refuses
// to chain a task to itself on failure (infinite-loop guard in FireChain:
// `targetID != completedTaskID`).
func TestEngine_OnFailureChain_NoLoopSelfReference(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	self := writeTask(t, dir, "self-loop",
		`export default async function main() { throw new Error("always fails") }`,
		task.TriggerConfig{Manual: true})
	self.OnFailureChain = ptrSpec("self-loop")
	_ = e.reg.Register(self)

	runID, err := e.engine.FireManual(context.Background(), "self-loop", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	// Give the chain dispatcher a window — if the self-reference guard
	// fails, this task would spawn additional runs of itself.
	time.Sleep(2 * time.Second)
	runs, err := e.engine.registry.ListRuns(context.Background(), "self-loop", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		// Surface IDs to help debug unexpected runs.
		ids := make([]string, 0, len(runs))
		for _, r := range runs {
			ids = append(ids, r.ID)
		}
		t.Errorf("self-loop produced %d runs (want 1): %s", len(runs), strings.Join(ids, ", "))
	}
}
