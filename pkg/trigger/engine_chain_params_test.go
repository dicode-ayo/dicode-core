package trigger

// Integration test for FireChain's chain.Params merge. Uses the same real-
// engine / Deno-runtime fixture pattern as engine_failure_chain_test.go.
// The chain target echoes its full `input` back as return value so we can
// inspect every key that the engine injected.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

func TestFireChain_MergesParamsIntoInput(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Chain target: echo the full input map back as the return value so we can
	// inspect the keys the engine injected.
	autoFix := writeTask(t, dir, "auto-fix-params",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(autoFix)

	// Failing task whose failure will fire the chain.
	failing := writeTask(t, dir, "will-fail-params",
		`export default async function main() { throw new Error("boom") }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(failing)

	// Configure defaults.on_failure_chain with user params.
	e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{
		Task:   "auto-fix-params",
		Params: map[string]any{"mode": "review", "max_iterations": 5},
	})

	// Fire the failing task and wait for it to reach terminal state.
	runID, err := e.engine.FireManual(context.Background(), "will-fail-params", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	primary := waitForTerminal(t, e.engine, runID, 30*time.Second)
	if primary.Status != "failure" {
		t.Fatalf("primary run status = %q, want failure", primary.Status)
	}

	// Wait for the chain target to complete.
	got := waitForRunOfTask(t, e.engine, "auto-fix-params", 30*time.Second)
	if got == nil {
		t.Fatal("auto-fix-params was not fired within the timeout")
	}
	if got.Status != "success" {
		t.Errorf("chain target status = %q, want success", got.Status)
	}

	// Decode the return value — the chain target echoed `input` back.
	var input map[string]any
	if err := json.Unmarshal([]byte(got.ReturnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", got.ReturnValue, err)
	}

	// Reserved keys must be present.
	if input["taskID"] != "will-fail-params" {
		t.Errorf("taskID = %v, want will-fail-params", input["taskID"])
	}
	if input["runID"] != runID {
		t.Errorf("runID = %v, want %s", input["runID"], runID)
	}
	if input["status"] != "failure" {
		t.Errorf("status = %v, want failure", input["status"])
	}
	// _chain_depth is always 1 in v1 (#238 tracks deeper guardrails).
	// JSON numbers decode as float64.
	if input["_chain_depth"] != float64(1) {
		t.Errorf("_chain_depth = %v (%T), want 1", input["_chain_depth"], input["_chain_depth"])
	}

	// User params must be merged in.
	if input["mode"] != "review" {
		t.Errorf("mode = %v, want review", input["mode"])
	}
	// max_iterations is int in Go but float64 after JSON round-trip.
	if input["max_iterations"] != float64(5) {
		t.Errorf("max_iterations = %v (%T), want 5", input["max_iterations"], input["max_iterations"])
	}
}

// TestFireChain_PerTaskFullyReplacesDefaults verifies that a per-task
// on_failure_chain fully replaces the engine-level defaults — there is NO
// deep-merge of params. Defaults' {mode: review, max_iterations: 5} must
// not bleed into a per-task chain that targets a different handler with no
// params.
func TestFireChain_PerTaskFullyReplacesDefaults(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Chain target for the DEFAULTS — should never run in this test.
	autoFix := writeTask(t, dir, "auto-fix-replace",
		`export default async function main() { return "should-not-run" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(autoFix)

	// Per-task chain target: echo the full input map back so we can inspect
	// every key the engine injected.
	differentHandler := writeTask(t, dir, "different-handler",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(differentHandler)

	// Configure DEFAULTS: auto-fix with params {mode: review, max_iterations: 5}.
	e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{
		Task:   "auto-fix-replace",
		Params: map[string]any{"mode": "review", "max_iterations": 5},
	})

	// Failing task with a PER-TASK on_failure_chain pointing at different-handler
	// with NO params — full replace, not a merge.
	failing := writeTask(t, dir, "user-task",
		`export default async function main() { throw new Error("boom") }`,
		task.TriggerConfig{Manual: true})
	override := &task.OnFailureChainSpec{Task: "different-handler"}
	failing.OnFailureChain = override
	_ = e.reg.Register(failing)

	// Fire the failing task and wait for it to reach terminal state.
	runID, err := e.engine.FireManual(context.Background(), "user-task", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	primary := waitForTerminal(t, e.engine, runID, 30*time.Second)
	if primary.Status != "failure" {
		t.Fatalf("primary run status = %q, want failure", primary.Status)
	}

	// Wait for the per-task chain target to complete.
	got := waitForRunOfTask(t, e.engine, "different-handler", 30*time.Second)
	if got == nil {
		t.Fatal("different-handler was not fired within the timeout")
	}
	if got.Status != "success" {
		t.Errorf("chain target status = %q, want success", got.Status)
	}

	// Decode the return value — the chain target echoed `input` back.
	var input map[string]any
	if err := json.Unmarshal([]byte(got.ReturnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", got.ReturnValue, err)
	}

	// Reserved keys must be present (the engine always injects these).
	if input["taskID"] != "user-task" {
		t.Errorf("taskID = %v, want user-task", input["taskID"])
	}

	// Defaults' params (mode, max_iterations) MUST NOT appear — per-task chain
	// fully replaces, it does not deep-merge with the defaults' Params.
	if _, ok := input["mode"]; ok {
		t.Errorf("defaults' mode leaked into per-task chain: input = %#v", input)
	}
	if _, ok := input["max_iterations"]; ok {
		t.Errorf("defaults' max_iterations leaked into per-task chain: input = %#v", input)
	}
}
