//go:build !windows

package python

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
)

// writePythonTask writes body to task.py in a fresh temp dir and returns a
// manual-trigger spec pointed at it.
func writePythonTask(t *testing.T, id, body string) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.py"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.Runtime("python"),
		TaskDir: dir,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 120 * time.Second,
	}
}

// newSuspendExecutor provisions uv the way the runtime does and returns a
// registry + executor, skipping when uv cannot be provisioned (offline CI).
func newSuspendExecutor(t *testing.T) (*registry.Registry, pkgruntime.Executor) {
	t.Helper()
	uv, err := uvpkg.EnsureUv("")
	if err != nil {
		t.Skipf("uv provisioning failed (offline?): %v", err)
	}
	rt, reg := newTestRuntime(t)
	return reg, rt.NewExecutor(uv)
}

// TestExecute_SuspendYieldsSuspendedResult runs a real Python task that calls
// dicode.suspend(state=..., schema=..., deadline=...) and asserts the run ends
// as a clean suspend (not a failure) with the state/schema/deadline captured on
// the RunResult (#512). The SuspendSignal must exit the process with code 0.
func TestExecute_SuspendYieldsSuspendedResult(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/wizard", `
async def main():
    dicode.suspend(
        state={"step": "ask_name", "n": 42},
        schema={
            "type": "object",
            "title": "Your name?",
            "properties": {"project_name": {"type": "string", "title": "Name"}},
            "required": ["project_name"],
        },
        deadline=1893456000000,
    )
    # Unreachable: suspend never returns.
    return {"reached": True}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID)
	if res.Error != nil {
		t.Fatalf("suspend must not be a failure, got error: %v\nlogs:\n%s", res.Error, joinLogs(logs))
	}
	if !res.Suspended {
		t.Fatalf("expected Suspended = true, got false\nlogs:\n%s", joinLogs(logs))
	}
	if res.ChainInput != nil {
		t.Errorf("suspend must not produce a return value, got %#v", res.ChainInput)
	}
	if res.ResumeDeadline != 1893456000000 {
		t.Errorf("ResumeDeadline = %d, want 1893456000000", res.ResumeDeadline)
	}

	var state struct {
		Step string `json:"step"`
		N    int    `json:"n"`
	}
	if err := json.Unmarshal(res.ResumeState, &state); err != nil {
		t.Fatalf("ResumeState not valid JSON (%q): %v", res.ResumeState, err)
	}
	if state.Step != "ask_name" || state.N != 42 {
		t.Errorf("ResumeState = %+v, want {ask_name 42}", state)
	}

	var schema struct {
		Title      string `json:"title"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(res.ResumeSchema, &schema); err != nil {
		t.Fatalf("ResumeSchema not valid JSON (%q): %v", res.ResumeSchema, err)
	}
	if schema.Title != "Your name?" || len(schema.Properties) != 1 || schema.Properties["project_name"].Type != "string" {
		t.Errorf("ResumeSchema = %+v, want title + one project_name property", schema)
	}
}

// TestExecute_SuspendAtTopLevelYieldsSuspendedResult covers a synchronous task
// that calls dicode.suspend() at module top level (no async main). The
// SuspendSignal unwinds past the return-capture epilogue and is caught by the
// SDK's sys.excepthook, which still exits the process cleanly (#95).
func TestExecute_SuspendAtTopLevelYieldsSuspendedResult(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/sync-wizard", `
if ctx.resume_state is None:
    dicode.suspend(
        state={"step": "sync"},
        schema={"type": "object", "properties": {"x": {"type": "string"}}},
    )
result = {"reached": True}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID)
	if res.Error != nil {
		t.Fatalf("top-level suspend must not be a failure, got: %v\nlogs:\n%s", res.Error, joinLogs(logs))
	}
	if !res.Suspended {
		t.Fatalf("expected Suspended = true\nlogs:\n%s", joinLogs(logs))
	}
	if res.ChainInput != nil {
		t.Errorf("suspend must not produce a return value, got %#v", res.ChainInput)
	}
	var state struct {
		Step string `json:"step"`
	}
	if err := json.Unmarshal(res.ResumeState, &state); err != nil {
		t.Fatalf("ResumeState not valid JSON (%q): %v", res.ResumeState, err)
	}
	if state.Step != "sync" {
		t.Errorf("ResumeState = %+v, want step=sync", state)
	}
}

// TestExecute_SuspendSwallowedByTaskFails guards the case where task code wraps
// dicode.suspend() in a broad try/except that swallows the SuspendSignal and
// keeps executing (#95). The payload is already recorded server-side, so a
// normal return would leave the run suspended-and-returned. The run must FAIL
// loudly and NOT be classified as suspended. Mirrors the Deno test.
func TestExecute_SuspendSwallowedByTaskFails(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/swallower", `
async def main():
    try:
        dicode.suspend(
            state={"step": "one"},
            schema={"type": "object", "properties": {"x": {"type": "string"}}},
        )
    except Exception:
        # Swallow the control-flow signal and keep going — the bug we guard against.
        pass
    return {"kept_running": True}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Suspended {
		t.Fatal("swallowed suspend must not be classified as suspended")
	}
	if res.Error == nil {
		t.Fatal("swallowed suspend must fail the run, got no error")
	}
	if res.ChainInput != nil {
		t.Errorf("swallowed suspend must not record a return value, got %#v", res.ChainInput)
	}

	logs, _ := reg.GetRunLogs(ctx, runID)
	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, "control-flow signal was caught by task code") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a clear diagnostic in the run log, logs:\n%s", joinLogs(logs))
	}
}

// TestExecute_ResumeStateAndInputExposed injects a prior-run state blob and a
// user form submission via RunOptions and asserts the task sees them on
// ctx.resume_state / ctx.resume_input (#95).
func TestExecute_ResumeStateAndInputExposed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/resumer", `
async def main():
    return {"st": ctx.resume_state, "inp": ctx.resume_input}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID:       runID,
		ResumeState: json.RawMessage(`{"step":"ask_framework","name":"proj"}`),
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID)
	if res.Error != nil {
		t.Fatalf("run error: %v\nlogs:\n%s", res.Error, joinLogs(logs))
	}
	if res.Suspended {
		t.Fatal("resume run must not be suspended")
	}

	ret, ok := res.ChainInput.(map[string]any)
	if !ok {
		t.Fatalf("ChainInput = %#v, want map", res.ChainInput)
	}
	st, ok := ret["st"].(map[string]any)
	if !ok {
		t.Fatalf("resume_state not exposed: %#v", ret["st"])
	}
	if st["step"] != "ask_framework" || st["name"] != "proj" {
		t.Errorf("resume_state = %#v, want step=ask_framework name=proj", st)
	}
	inp, ok := ret["inp"].(map[string]any)
	if !ok {
		t.Fatalf("resume_input not exposed: %#v", ret["inp"])
	}
	if inp["project_name"] != "acme" {
		t.Errorf("resume_input = %#v, want project_name=acme", inp)
	}
}

// TestExecute_NoResumeIsNone verifies that a first (non-resume) invocation sees
// ctx.resume_state / ctx.resume_input as None (#95), so a task can branch on
// their presence.
func TestExecute_NoResumeIsNone(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/firstrun", `
async def main():
    return {"has_state": ctx.resume_state is not None, "has_input": ctx.resume_input is not None}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID)
	if res.Error != nil {
		t.Fatalf("run error: %v\nlogs:\n%s", res.Error, joinLogs(logs))
	}
	ret, ok := res.ChainInput.(map[string]any)
	if !ok {
		t.Fatalf("ChainInput = %#v, want map", res.ChainInput)
	}
	if ret["has_state"] != false || ret["has_input"] != false {
		t.Errorf("expected resume_state/resume_input None on first run, got %#v", ret)
	}
}
