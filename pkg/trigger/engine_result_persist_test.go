package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestEngine_RunResult_Disabled_SuppressesPersistence verifies the new
// `run_result: { enabled: false }` flag suppresses the JSON-marshalled
// return value from being persisted to `runs.return_value`, while still
// allowing structured output_content to be persisted.
func TestEngine_RunResult_Disabled_SuppressesPersistence(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	disabled := false
	spec := writeTask(t, dir, "secret-emitter",
		`export default async function main() { return { token: "ya29.sensitive" } }`,
		task.TriggerConfig{Manual: true})
	spec.RunResult = &task.RunResultConfig{Enabled: &disabled}
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "secret-emitter", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitForTerminal(t, e.engine, runID, 30*time.Second)
	if run.Status != registry.StatusSuccess {
		t.Fatalf("status = %q, want success", run.Status)
	}
	if run.ReturnValue != "" {
		t.Errorf("ReturnValue should be empty when run_result.enabled=false, got %q", run.ReturnValue)
	}
}

// TestEngine_RunResult_Default_PersistsValue is the baseline: without the
// flag set the same task body produces a populated return_value column.
// This guards against accidentally suppressing persistence for everyone.
func TestEngine_RunResult_Default_PersistsValue(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "plain-emitter",
		`export default async function main() { return { token: "ya29.sensitive" } }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "plain-emitter", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	run := waitForTerminal(t, e.engine, runID, 30*time.Second)
	if run.Status != registry.StatusSuccess {
		t.Fatalf("status = %q, want success", run.Status)
	}
	if run.ReturnValue == "" {
		t.Errorf("ReturnValue should be populated by default; got empty")
	}
}

// TestEngine_RunResult_Disabled_WaitRunStillDelivers verifies the in-memory
// delivery contract: even when persistence is suppressed, synchronous
// callers (dicode.run_task -> WaitRun) still see the return value. This is
// the load-bearing invariant — without it, dicode.run_task would silently
// return nil for any non-persisted task.
func TestEngine_RunResult_Disabled_WaitRunStillDelivers(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	disabled := false
	spec := writeTask(t, dir, "wait-secret",
		`export default async function main() { return { token: "ya29.in-memory" } }`,
		task.TriggerConfig{Manual: true})
	spec.RunResult = &task.RunResultConfig{Enabled: &disabled}
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "wait-secret", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := e.engine.WaitRun(ctx, runID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if res.Status != registry.StatusSuccess {
		t.Fatalf("status = %q, want success", res.Status)
	}
	m, ok := res.ReturnValue.(map[string]interface{})
	if !ok {
		t.Fatalf("WaitRun ReturnValue is %T, want map; value=%v", res.ReturnValue, res.ReturnValue)
	}
	if m["token"] != "ya29.in-memory" {
		t.Errorf("WaitRun lost the token: got %v", m["token"])
	}

	// And confirm the DB row still shows it was suppressed (defence in depth).
	run, err := e.reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.ReturnValue != "" {
		t.Errorf("DB ReturnValue should be empty; got %q", run.ReturnValue)
	}
}

// TestEngine_RunResult_Disabled_ChainStillReceivesOutput verifies that
// chained downstream tasks still receive the upstream task's return value
// through `input.output`, even when persistence is suppressed. FireChain
// already routes through the in-memory ChainInput on the RunResult, so this
// test guards against future refactors accidentally tying chain delivery
// to the persisted column.
func TestEngine_RunResult_Disabled_ChainStillReceivesOutput(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	disabled := false

	upstream := writeTask(t, dir, "chain-up",
		`export default async function main() { return { msg: "from-up" } }`,
		task.TriggerConfig{Manual: true})
	upstream.RunResult = &task.RunResultConfig{Enabled: &disabled}

	downstream := writeTask(t, dir, "chain-down",
		`export default async function main({ input }) { return "got:" + input.msg }`,
		task.TriggerConfig{Chain: &task.ChainTrigger{From: "chain-up", On: "success"}})

	_ = e.reg.Register(upstream)
	_ = e.reg.Register(downstream)

	upRunID, err := e.engine.FireManual(context.Background(), "chain-up", nil)
	if err != nil {
		t.Fatalf("FireManual upstream: %v", err)
	}
	waitForTerminal(t, e.engine, upRunID, 30*time.Second)

	// Wait for the chained downstream run to appear and complete. Poll
	// until a downstream run with our parent_run_id finishes successfully
	// — chain firing is goroutine-driven so it lands shortly after the
	// upstream cleanup runs.
	deadline := time.Now().Add(60 * time.Second)
	var downRun *registry.Run
	for time.Now().Before(deadline) {
		runs, err := e.reg.ListRuns(context.Background(), "chain-down", 5)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		for _, r := range runs {
			if r.Status == registry.StatusSuccess {
				downRun = r
				break
			}
		}
		if downRun != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if downRun == nil {
		t.Fatal("downstream chain run never reached success")
	}
	// Downstream's ReturnValue should embed the upstream payload.
	if downRun.ReturnValue == "" {
		t.Fatal("downstream ReturnValue empty — chain input never made it through")
	}
	// JSON-encoded string: "\"got:from-up\""
	want := `"got:from-up"`
	if downRun.ReturnValue != want {
		t.Errorf("downstream ReturnValue = %q, want %q", downRun.ReturnValue, want)
	}
}
