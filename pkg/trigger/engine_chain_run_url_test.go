package trigger

// buildChainPayload stamps a run_url key (linking to the upstream run that
// fired the chain) built through Engine.SetRunURLFunc, the same injection
// point the suspend notifier's resumeURL uses (pkg/daemon/daemon.go).
// Mirrors the existing engine_chain_params_test.go /
// engine_success_chain_params_test.go pattern.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestFireChain_Success_RunURLStampedWhenConfigured verifies the
// trigger.chain path stamps run_url, built from the completed (upstream)
// run's ID, when SetRunURLFunc is wired.
func TestFireChain_Success_RunURLStampedWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	e.engine.SetRunURLFunc(func(runID string) string { return "https://dicode.example/?run=" + runID })

	downstream := writeTask(t, dir, "downstream-run-url",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{
			Chain: &task.ChainTrigger{
				From:   "upstream-run-url",
				On:     "success",
				Params: map[string]any{"mode": "prod"},
			},
		})
	_ = e.reg.Register(downstream)

	upstream := writeTask(t, dir, "upstream-run-url",
		`export default async function main() { return "ok" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(upstream)

	upstreamRunID, err := e.engine.FireManual(context.Background(), "upstream-run-url", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	if primary := waitForTerminal(t, e.engine, upstreamRunID, 30*time.Second); primary.Status != registry.StatusSuccess {
		t.Fatalf("upstream status = %q, want success", primary.Status)
	}

	got := waitForRunOfTask(t, e.engine, "downstream-run-url", 30*time.Second)
	if got == nil {
		t.Fatal("downstream-run-url was not fired within the timeout")
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(got.ReturnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", got.ReturnValue, err)
	}

	want := "https://dicode.example/?run=" + upstreamRunID
	if input["run_url"] != want {
		t.Errorf("run_url = %v, want %v", input["run_url"], want)
	}
	// run_url links the upstream run, not whatever ID the downstream itself
	// was fired with.
	if input["runID"] != upstreamRunID {
		t.Errorf("runID = %v, want %v", input["runID"], upstreamRunID)
	}
}

// TestFireChain_Failure_RunURLStampedWhenConfigured is the on_failure_chain
// analogue: the shared buildChainPayload kernel must stamp run_url on the
// failure path too.
func TestFireChain_Failure_RunURLStampedWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	e.engine.SetRunURLFunc(func(runID string) string { return "https://dicode.example/?run=" + runID })

	fixer := writeTask(t, dir, "fixer-run-url",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(fixer)

	failing := writeTask(t, dir, "will-fail-run-url",
		`export default async function main() { throw new Error("boom") }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(failing)

	if err := e.engine.SetDefaultsOnFailureChain(task.OnFailureChainSpec{Task: "fixer-run-url"}); err != nil {
		t.Fatalf("SetDefaultsOnFailureChain: %v", err)
	}

	runID, err := e.engine.FireManual(context.Background(), "will-fail-run-url", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	if primary := waitForTerminal(t, e.engine, runID, 30*time.Second); primary.Status != registry.StatusFailure {
		t.Fatalf("primary status = %q, want failure", primary.Status)
	}

	got := waitForRunOfTask(t, e.engine, "fixer-run-url", 30*time.Second)
	if got == nil {
		t.Fatal("fixer-run-url was not fired within the timeout")
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(got.ReturnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", got.ReturnValue, err)
	}

	want := "https://dicode.example/?run=" + runID
	if input["run_url"] != want {
		t.Errorf("run_url = %v, want %v", input["run_url"], want)
	}
}

// TestFireChain_RunURLOmittedWhenNotConfigured pins the degrade-to-absent
// rule: with no SetRunURLFunc wired (the default — e.g. a daemon boot path
// that hasn't reached the webui step, or these very tests' baseline), the
// payload must omit run_url entirely rather than stamp a broken link.
func TestFireChain_RunURLOmittedWhenNotConfigured(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	// No SetRunURLFunc call — this is the field's zero value.

	downstream := writeTask(t, dir, "downstream-no-run-url",
		`export default async function main({ input }) { return input }`,
		task.TriggerConfig{
			Chain: &task.ChainTrigger{
				From:   "upstream-no-run-url",
				On:     "success",
				Params: map[string]any{"mode": "prod"},
			},
		})
	_ = e.reg.Register(downstream)

	upstream := writeTask(t, dir, "upstream-no-run-url",
		`export default async function main() { return "ok" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(upstream)

	if _, err := e.engine.FireManual(context.Background(), "upstream-no-run-url", nil); err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	got := waitForRunOfTask(t, e.engine, "downstream-no-run-url", 30*time.Second)
	if got == nil {
		t.Fatal("downstream-no-run-url was not fired within the timeout")
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(got.ReturnValue), &input); err != nil {
		t.Fatalf("unmarshal return value %q: %v", got.ReturnValue, err)
	}
	if _, present := input["run_url"]; present {
		t.Errorf("run_url = %v, want key absent", input["run_url"])
	}
}
