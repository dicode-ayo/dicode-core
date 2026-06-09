package trigger

// Integration tests for FireChain's success-path Params merge. Mirrors
// engine_chain_params_test.go (failure-chain side) but exercises the
// declared trigger.chain → spec.Trigger.Chain.Params path instead of
// on_failure_chain.
//
// Two cases:
//
//  1. No params on trigger.chain: downstream sees the raw upstream output as
//     its `input` argument (existing contract — backwards compat).
//
//  2. Params set on trigger.chain: downstream sees a wrapped map with
//     engine-reserved keys (taskID, runID, status, output) merged on top of
//     user-supplied params.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

func TestFireChain_Success_ParamsMergedIntoInput(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Downstream task: declares trigger.chain on "upstream-params" + user
	// params. Echoes the full `input` back as its return value so the test
	// can inspect every key the engine injected.
	downstream := writeTask(t, dir, "downstream-params",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{
			Chain: &task.ChainTrigger{
				From:   "upstream-params",
				On:     "success",
				Params: map[string]any{"mode": "prod", "shard": "3"},
			},
		})
	_ = e.reg.Register(downstream)

	// Upstream task: returns a string. With Params set the engine should
	// wrap this in the downstream's input map under the "output" key.
	upstream := writeTask(t, dir, "upstream-params",
		`export default async function main() { return "rendered-yaml-content" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(upstream)

	// Fire upstream and wait for it to reach terminal state.
	upstreamRunID, err := e.engine.FireManual(context.Background(), "upstream-params", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	primary := waitForTerminal(t, e.engine, upstreamRunID, 30*time.Second)
	if primary.Status != registry.StatusSuccess {
		t.Fatalf("upstream status = %q, want success", primary.Status)
	}

	// Wait for the downstream chain target to complete.
	got := waitForRunOfTask(t, e.engine, "downstream-params", 30*time.Second)
	if got == nil {
		t.Fatal("downstream-params was not fired within the timeout")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("downstream status = %q, want success", got.Status)
	}

	returnValue := got.ReturnValue
	var input map[string]any
	if err := json.Unmarshal([]byte(returnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", returnValue, err)
	}

	// Reserved keys must be present.
	if input["taskID"] != "upstream-params" {
		t.Errorf("taskID = %v, want upstream-params", input["taskID"])
	}
	if input["runID"] != upstreamRunID {
		t.Errorf("runID = %v, want %s", input["runID"], upstreamRunID)
	}
	if input["status"] != "success" {
		t.Errorf("status = %v, want success", input["status"])
	}
	if input["output"] != "rendered-yaml-content" {
		t.Errorf("output = %v, want rendered-yaml-content", input["output"])
	}

	// User params must be merged in.
	if input["mode"] != "prod" {
		t.Errorf("mode = %v, want prod", input["mode"])
	}
	if input["shard"] != "3" {
		t.Errorf("shard = %v, want 3", input["shard"])
	}
}

// TestFireChain_Success_NoParams_PreservesRawOutput verifies the
// backwards-compat path: when trigger.chain has no Params, downstream
// receives the upstream's raw return value as `input` — same as today's
// behavior. This protects tasks that consume `input` as the upstream's
// output directly rather than as a map.
func TestFireChain_Success_NoParams_PreservesRawOutput(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Downstream: no Params; echoes input back so we can inspect it.
	downstream := writeTask(t, dir, "downstream-noparams",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{
			Chain: &task.ChainTrigger{
				From: "upstream-noparams",
				On:   "success",
			},
		})
	_ = e.reg.Register(downstream)

	// Upstream emits a string. Engine must forward it as-is.
	upstream := writeTask(t, dir, "upstream-noparams",
		`export default async function main() { return "raw-value" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(upstream)

	runID, err := e.engine.FireManual(context.Background(), "upstream-noparams", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	primary := waitForTerminal(t, e.engine, runID, 30*time.Second)
	if primary.Status != registry.StatusSuccess {
		t.Fatalf("upstream status = %q, want success", primary.Status)
	}

	got := waitForRunOfTask(t, e.engine, "downstream-noparams", 30*time.Second)
	if got == nil {
		t.Fatal("downstream-noparams was not fired within the timeout")
	}
	if got.Status != registry.StatusSuccess {
		t.Errorf("downstream status = %q, want success", got.Status)
	}

	returnValue := got.ReturnValue
	// No wrapping: downstream sees the raw upstream output as a JSON string.
	var input any
	if err := json.Unmarshal([]byte(returnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", returnValue, err)
	}
	if input != "raw-value" {
		t.Errorf("input = %v (%T), want raw-value (no wrapping when params empty)", input, input)
	}
}
