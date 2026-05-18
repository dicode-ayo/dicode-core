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

	"github.com/dicode/dicode/pkg/registry"
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
	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{
		Task:   "auto-fix-params",
		Params: map[string]any{"mode": "review", "max_iterations": 5},
	}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

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
	// Poll for return_value: the runtime's deferred FinishRun commits before
	// the engine wrapper's SetRunResult, so a fast reader can see status=success
	// with an empty ReturnValue.
	returnValue := pollReturnValue(t, e.engine, got.ID, 5*time.Second)
	var input map[string]any
	if err := json.Unmarshal([]byte(returnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", returnValue, err)
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
	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{
		Task:   "auto-fix-replace",
		Params: map[string]any{"mode": "review", "max_iterations": 5},
	}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

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
	// Poll for return_value: the runtime's deferred FinishRun commits before
	// the engine wrapper's SetRunResult, so a fast reader can see status=success
	// with an empty ReturnValue.
	returnValue := pollReturnValue(t, e.engine, got.ID, 5*time.Second)
	var input map[string]any
	if err := json.Unmarshal([]byte(returnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", returnValue, err)
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

// TestFireChain_SuppressesChainOfChains verifies that when a chain target
// itself fails with on_failure_chain configured, the engine does NOT fire
// a second chain — the chain-of-chains suppression guard kicks in.
func TestFireChain_SuppressesChainOfChains(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// "auto-fix-loop" always fails and has on_failure_chain pointing back
	// to itself via defaults. Without the guard this would recurse infinitely.
	autoFixLoop := writeTask(t, dir, "auto-fix-loop",
		`export default async function main() { throw new Error("always fails") }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(autoFixLoop)

	// Primary task: fails, fires auto-fix-loop via defaults.
	primary := writeTask(t, dir, "chain-primary",
		`export default async function main() { throw new Error("primary fails") }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(primary)

	// defaults.on_failure_chain → auto-fix-loop.
	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{Task: "auto-fix-loop"}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

	// Fire the primary task.
	runID, err := e.engine.FireManual(context.Background(), "chain-primary", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	// auto-fix-loop should be fired exactly once (for the primary failure).
	// The suppression guard must prevent it from chain-firing again when
	// auto-fix-loop itself fails.
	chainRun := waitForRunOfTask(t, e.engine, "auto-fix-loop", 15*time.Second)
	if chainRun == nil {
		t.Fatal("auto-fix-loop was never fired for the primary failure")
	}

	// Give a window for a (suppressed) second chain fire to (incorrectly) land.
	time.Sleep(3 * time.Second)

	runs, err := e.engine.registry.ListRuns(context.Background(), "auto-fix-loop", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		ids := make([]string, 0, len(runs))
		for _, r := range runs {
			ids = append(ids, r.ID)
		}
		t.Errorf("chain-of-chains: auto-fix-loop ran %d times (want 1): %v", len(runs), ids)
	}
}

// TestFireChain_SetsParentRunIDOnChainedRun verifies that the chained run's
// ParentRunID field is set to the failing run's ID. This is required for the
// WebUI run-tree view and for downstream correlation (e.g. the auto-fix loop's
// runs.replay primitive in #234 looks up parent runs).
func TestFireChain_SetsParentRunIDOnChainedRun(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Chain target: echo input back so the run reaches success.
	chainTarget := writeTask(t, dir, "chain-target-parent",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(chainTarget)

	// Failing task whose failure will fire the on_failure_chain.
	failing := writeTask(t, dir, "user-task-parent",
		`export default async function main() { throw new Error("intentional") }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(failing)

	// Configure defaults.on_failure_chain pointing at chain-target-parent.
	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{
		Task: "chain-target-parent",
	}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

	// Fire the failing task and wait for terminal state.
	failedRunID, err := e.engine.FireManual(context.Background(), "user-task-parent", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	primary := waitForTerminal(t, e.engine, failedRunID, 30*time.Second)
	if primary.Status != "failure" {
		t.Fatalf("primary run status = %q, want failure", primary.Status)
	}

	// Wait for the chained run to complete.
	chainedRun := waitForRunOfTask(t, e.engine, "chain-target-parent", 30*time.Second)
	if chainedRun == nil {
		t.Fatal("chain-target-parent was not fired within the timeout")
	}

	// Assert that ParentRunID was threaded through from the failing run.
	if chainedRun.ParentRunID != failedRunID {
		t.Errorf("ParentRunID = %q, want %q", chainedRun.ParentRunID, failedRunID)
	}
}

// TestChainDispatch_ResolvesInputOutput verifies the dispatch-time
// substitution of ${input.output} in trigger.chain.params with the
// upstream's string return value.
func TestChainDispatch_ResolvesInputOutput(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Downstream task: declares trigger.chain on "upstream-render" with a
	// param whose value is the literal ${input.output} token. Echoes the
	// full input map back so we can inspect the resolved param.
	downstream := writeTask(t, dir, "downstream-interp",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{
			Chain: &task.ChainTrigger{
				From:   "upstream-render",
				On:     "success",
				Params: map[string]any{"content": "${input.output}", "path": "/tmp/foo"},
			},
		})
	_ = e.reg.Register(downstream)

	// Upstream returns a string; the engine must substitute that into the
	// downstream's `content` param before dispatching.
	upstream := writeTask(t, dir, "upstream-render",
		`export default async function main() { return "rendered-string" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(upstream)

	upstreamRunID, err := e.engine.FireManual(context.Background(), "upstream-render", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	primary := waitForTerminal(t, e.engine, upstreamRunID, 30*time.Second)
	if primary.Status != "success" {
		t.Fatalf("upstream status = %q, want success", primary.Status)
	}

	got := waitForRunOfTask(t, e.engine, "downstream-interp", 30*time.Second)
	if got == nil {
		t.Fatal("downstream-interp was not fired within the timeout")
	}
	if got.Status != "success" {
		t.Errorf("downstream status = %q, want success", got.Status)
	}

	returnValue := pollReturnValue(t, e.engine, got.ID, 5*time.Second)
	var input map[string]any
	if err := json.Unmarshal([]byte(returnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", returnValue, err)
	}

	// content should have been substituted; path should be untouched.
	if input["content"] != "rendered-string" {
		t.Errorf("content = %v, want rendered-string (token was not resolved)", input["content"])
	}
	if input["path"] != "/tmp/foo" {
		t.Errorf("path = %v, want /tmp/foo", input["path"])
	}
}

// strPtr is a tiny helper for the TestCoerceStringReturn table — Go has no
// literal syntax for "address of a string literal" so the typed-nil and
// pointer-to-string cases need a helper to construct the *string.
func strPtr(s string) *string { return &s }

// TestOnFailureChainDispatch_ResolvesInputOutput drives FireChain
// directly with a controlled string `output` to verify that the
// failure-chain dispatch resolves ${input.output} in
// on_failure_chain.params. Mirrors the success-chain interpolation
// test (TestChainDispatch_ResolvesInputOutput) — both chain edges
// must support the same contract; a literal token reaching the
// downstream is a footgun regardless of which path triggered.
//
// We invoke FireChain directly (rather than driving a full deno-runtime
// run through a thrown error) because thrown-Error tasks produce nil
// ChainInput in practice — the upstream never reaches the `/return`
// channel that populates ReturnValue. Calling FireChain with a chosen
// `output` is the cleanest way to pin the resolver wiring.
func TestOnFailureChainDispatch_ResolvesInputOutput(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Failure-chain target: echoes the full input map so we can inspect
	// the resolved param.
	fallback := writeTask(t, dir, "fallback-interp",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(fallback)

	// Source of the failure — only its on_failure_chain field matters
	// for this test; we don't actually run it.
	source := writeTask(t, dir, "source-interp",
		`export default async function main() { return "unused" }`,
		task.TriggerConfig{Manual: true})
	source.OnFailureChain = &task.OnFailureChainSpec{
		Task:   "fallback-interp",
		Params: map[string]any{"content": "${input.output}", "path": "/tmp/foo"},
	}
	_ = e.reg.Register(source)

	// Synthesise a parent run row so FireChain has a real runID to
	// reference (runTriggerSource lookup tolerates a missing entry, so
	// we don't need to populate it; the run row itself just needs to
	// exist for the registry to satisfy SetRunGroup / parent lookups
	// downstream).
	parentRunID, err := e.reg.StartRunWithID(
		context.Background(), "parent-run", "source-interp", "", string(registry.TriggerManual),
	)
	if err != nil {
		t.Fatalf("seed parent run: %v", err)
	}
	if err := e.reg.FinishRun(context.Background(), parentRunID, registry.StatusFailure); err != nil {
		t.Fatalf("finish parent run: %v", err)
	}

	// Drive FireChain with a string output. The resolver should
	// substitute that string into chainSpec.Params["content"].
	e.engine.FireChain(context.Background(), "source-interp", parentRunID, registry.StatusFailure, "rendered-error-output")

	got := waitForRunOfTask(t, e.engine, "fallback-interp", 30*time.Second)
	if got == nil {
		t.Fatal("fallback-interp was not fired within the timeout")
	}
	if got.Status != "success" {
		t.Errorf("fallback status = %q, want success", got.Status)
	}

	returnValue := pollReturnValue(t, e.engine, got.ID, 5*time.Second)
	var input map[string]any
	if err := json.Unmarshal([]byte(returnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", returnValue, err)
	}

	// content should have been substituted with the upstream's string
	// return; path should be untouched. The exact contract that PR #310
	// is shipping for the on_failure_chain edge.
	if input["content"] != "rendered-error-output" {
		t.Errorf("content = %v, want rendered-error-output (token was not resolved on the failure-chain edge)", input["content"])
	}
	if input["path"] != "/tmp/foo" {
		t.Errorf("path = %v, want /tmp/foo", input["path"])
	}
}

// TestOnFailureChainDispatch_NonStringUpstreamSkips verifies that an
// upstream whose return value is non-string (e.g. a map, a thrown
// non-string value, or nil — which is what real JS-throw failures
// produce in practice) causes the failure-chain dispatch to be
// skipped: the literal ${input.output} token must NOT pass through
// unresolved.
//
// PR2's coerceStringReturn() turns non-string returns into "", which
// propagates as ErrInputUnavailable through ResolveInputOutputMap, which
// causes the dispatch to log + return without firing the downstream.
func TestOnFailureChainDispatch_NonStringUpstreamSkips(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Failure-chain target: would echo the input back if it ran.
	fallback := writeTask(t, dir, "fallback-skip",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(fallback)

	source := writeTask(t, dir, "source-skip",
		`export default async function main() { return "unused" }`,
		task.TriggerConfig{Manual: true})
	source.OnFailureChain = &task.OnFailureChainSpec{
		Task:   "fallback-skip",
		Params: map[string]any{"content": "${input.output}"},
	}
	_ = e.reg.Register(source)

	parentRunID, err := e.reg.StartRunWithID(
		context.Background(), "parent-skip-run", "source-skip", "", string(registry.TriggerManual),
	)
	if err != nil {
		t.Fatalf("seed parent run: %v", err)
	}
	if err := e.reg.FinishRun(context.Background(), parentRunID, registry.StatusFailure); err != nil {
		t.Fatalf("finish parent run: %v", err)
	}

	// Non-string output: a map. coerceStringReturn returns "", resolver
	// fails, dispatch must skip.
	e.engine.FireChain(context.Background(), "source-skip", parentRunID, registry.StatusFailure,
		map[string]any{"nested": "object"})

	// Give the chain dispatcher a window to (incorrectly) fire.
	time.Sleep(2 * time.Second)
	runs, err := e.engine.registry.ListRuns(context.Background(), "fallback-skip", 5)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) > 0 {
		t.Errorf("fallback-skip should not have run when ${input.output} cannot resolve; got %d runs", len(runs))
	}
}

// TestCoerceStringReturn pins the helper's contract: only direct-string
// returns flow through; everything else (maps, slices, numbers, nil,
// typed-nil interfaces, pointers-to-string) becomes "". The empty string
// then propagates as ErrInputUnavailable through the resolver, which is
// the loud-failure path we want for non-string upstreams.
func TestCoerceStringReturn(t *testing.T) {
	var typedNilString *string // (*string)(nil); interface-wraps to a typed-nil
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"direct string", "hello", "hello"},
		{"empty string", "", ""},
		{"map", map[string]interface{}{"x": 1}, ""},
		{"slice", []interface{}{1, 2}, ""},
		{"number", 42, ""},
		{"nil", nil, ""},
		{"typed nil interface", typedNilString, ""},
		{"pointer to string", strPtr("hello"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coerceStringReturn(c.in); got != c.want {
				t.Errorf("coerceStringReturn(%v) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}
