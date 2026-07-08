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
// the RunResult (#512). The runner persists `state` wrapped in a { __step, state }
// envelope; the SuspendSignal must exit the process with code 0.
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

	// ResumeState is the internal envelope: __step null (no `to`), the author's
	// blob nested under `state`.
	var env struct {
		Step  *string `json:"__step"`
		State struct {
			Step string `json:"step"`
			N    int    `json:"n"`
		} `json:"state"`
	}
	if err := json.Unmarshal(res.ResumeState, &env); err != nil {
		t.Fatalf("ResumeState not valid JSON (%q): %v", res.ResumeState, err)
	}
	if env.Step != nil {
		t.Errorf("__step = %v, want null for a two-function/main suspend", *env.Step)
	}
	if env.State.Step != "ask_name" || env.State.N != 42 {
		t.Errorf("unwrapped state = %+v, want {ask_name 42}", env.State)
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
// that calls dicode.suspend() at module top level (no main). The SuspendSignal
// unwinds past the return-capture epilogue and is caught by the SDK's
// sys.excepthook, which still exits the process cleanly (#95).
func TestExecute_SuspendAtTopLevelYieldsSuspendedResult(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/sync-wizard", `
if ctx.state is None:
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
	var env struct {
		State struct {
			Step string `json:"step"`
		} `json:"state"`
	}
	if err := json.Unmarshal(res.ResumeState, &env); err != nil {
		t.Fatalf("ResumeState not valid JSON (%q): %v", res.ResumeState, err)
	}
	if env.State.Step != "sync" {
		t.Errorf("unwrapped state = %+v, want step=sync", env.State)
	}
}

// TestExecute_MainOnlyResumeReRunsMain is the single-main back-compat guard: a
// task defining only `main` re-runs `main` on resume, its author state round-trips
// through the __step envelope invisibly, and the handler reads ctx.state /
// ctx.input from the module-global ctx (#512).
func TestExecute_MainOnlyResumeReRunsMain(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/loopguard", `
async def main():
    if ctx.state is None:
        dicode.suspend(
            state={"step": "ask_name"},
            schema={"type": "object", "properties": {"project_name": {"type": "string"}}, "required": ["project_name"]},
        )
    return {"defined": ctx.state is not None, "step": ctx.state["step"], "name": ctx.input["project_name"]}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute (suspend): %v", err)
	}
	if !first.Suspended || len(first.ResumeState) == 0 {
		t.Fatalf("first run must suspend and capture a state blob; got suspended=%v", first.Suspended)
	}

	runID2, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID:       runID2,
		Resumed:     true,
		ResumeState: json.RawMessage(first.ResumeState),
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Execute (resume): %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID2)
	if second.Suspended {
		t.Fatalf("resume must not re-suspend\nlogs:\n%s", joinLogs(logs))
	}
	ret, ok := second.ChainInput.(map[string]any)
	if !ok {
		t.Fatalf("ChainInput = %#v, want map\nlogs:\n%s", second.ChainInput, joinLogs(logs))
	}
	if ret["defined"] != true {
		t.Errorf("ctx.state must be defined on resume, got %#v", ret["defined"])
	}
	if ret["step"] != "ask_name" {
		t.Errorf("ctx.state[step] = %#v, want ask_name", ret["step"])
	}
	if ret["name"] != "acme" {
		t.Errorf("ctx.input[project_name] = %#v, want acme", ret["name"])
	}
}

// TestExecute_ResumeDispatchesToResumeFn covers the two-function shape: main
// (no args, reads global ctx) suspends, and `resume(ctx)` (arg style) runs on
// the continuation with the carried state + validated input (#512).
func TestExecute_ResumeDispatchesToResumeFn(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/twofunc", `
async def main():
    dicode.suspend(
        state={"greeting": "hi"},
        schema={"type": "object", "properties": {"project_name": {"type": "string"}}, "required": ["project_name"]},
    )
    return {"via": "main"}

async def resume(ctx):
    return {"via": "resume", "greeting": ctx.state["greeting"], "name": ctx.input["project_name"]}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute (suspend): %v", err)
	}
	if !first.Suspended {
		t.Fatalf("first run must suspend")
	}

	runID2, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID:       runID2,
		Resumed:     true,
		ResumeState: json.RawMessage(first.ResumeState),
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Execute (resume): %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID2)
	if second.Suspended {
		t.Fatalf("resume must not re-suspend\nlogs:\n%s", joinLogs(logs))
	}
	ret, ok := second.ChainInput.(map[string]any)
	if !ok {
		t.Fatalf("ChainInput = %#v, want map\nlogs:\n%s", second.ChainInput, joinLogs(logs))
	}
	if ret["via"] != "resume" {
		t.Errorf("dispatch went to %#v, want the resume handler", ret["via"])
	}
	if ret["greeting"] != "hi" {
		t.Errorf("ctx.state[greeting] = %#v, want hi", ret["greeting"])
	}
	if ret["name"] != "acme" {
		t.Errorf("ctx.input[project_name] = %#v, want acme", ret["name"])
	}
}

// TestExecute_StepsWizardMultiStep drives a full named-step wizard (#512): main
// suspends to="choose_framework", that step sees ctx.input and suspends
// to="deploy" carrying state, and deploy sees the prior state + the new input
// and returns. Step handlers take ctx as an argument.
func TestExecute_StepsWizardMultiStep(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/steps-wizard", `
async def main():
    assert ctx.state is None, "main must see ctx.state None on the first run"
    dicode.suspend(
        to="choose_framework",
        schema={"type": "object", "properties": {"framework": {"type": "string"}}, "required": ["framework"]},
    )
    return {"via": "main"}

async def choose_framework(ctx):
    dicode.suspend(
        to="deploy",
        state={"framework": ctx.input["framework"]},
        schema={"type": "object", "properties": {"env": {"type": "string"}}, "required": ["env"]},
    )
    return {"via": "choose_framework"}

async def deploy(ctx):
    return {"framework": ctx.state["framework"], "env": ctx.input["env"]}

steps = {"choose_framework": choose_framework, "deploy": deploy}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	// Run 1 — main suspends to choose_framework.
	runID1, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	r1, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID1})
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	logs1, _ := reg.GetRunLogs(ctx, runID1)
	if !r1.Suspended {
		t.Fatalf("run 1 must suspend\nlogs:\n%s", joinLogs(logs1))
	}
	var env1 struct {
		Step string `json:"__step"`
	}
	if err := json.Unmarshal(r1.ResumeState, &env1); err != nil {
		t.Fatalf("run 1 ResumeState: %v", err)
	}
	if env1.Step != "choose_framework" {
		t.Fatalf("__step = %q, want choose_framework", env1.Step)
	}

	// Run 2 — choose_framework sees the framework input, suspends to deploy.
	runID2, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID:       runID2,
		Resumed:     true,
		ResumeState: json.RawMessage(r1.ResumeState),
		ResumeInput: json.RawMessage(`{"framework":"deno"}`),
	})
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	logs2, _ := reg.GetRunLogs(ctx, runID2)
	if !r2.Suspended {
		t.Fatalf("run 2 must suspend again\nlogs:\n%s", joinLogs(logs2))
	}
	var env2 struct {
		Step  string `json:"__step"`
		State struct {
			Framework string `json:"framework"`
		} `json:"state"`
	}
	if err := json.Unmarshal(r2.ResumeState, &env2); err != nil {
		t.Fatalf("run 2 ResumeState: %v", err)
	}
	if env2.Step != "deploy" {
		t.Errorf("__step = %q, want deploy", env2.Step)
	}
	if env2.State.Framework != "deno" {
		t.Errorf("carried state.framework = %q, want deno", env2.State.Framework)
	}

	// Run 3 — deploy sees the prior state + the new env input and finishes.
	runID3, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	r3, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID:       runID3,
		Resumed:     true,
		ResumeState: json.RawMessage(r2.ResumeState),
		ResumeInput: json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatalf("Run 3: %v", err)
	}
	logs3, _ := reg.GetRunLogs(ctx, runID3)
	if r3.Suspended {
		t.Fatalf("run 3 must finish, not suspend\nlogs:\n%s", joinLogs(logs3))
	}
	ret, ok := r3.ChainInput.(map[string]any)
	if !ok {
		t.Fatalf("ChainInput = %#v, want map\nlogs:\n%s", r3.ChainInput, joinLogs(logs3))
	}
	if ret["framework"] != "deno" {
		t.Errorf("deploy ctx.state[framework] = %#v, want deno", ret["framework"])
	}
	if ret["env"] != "prod" {
		t.Errorf("deploy ctx.input[env] = %#v, want prod", ret["env"])
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

// TestExecute_ResumeStateAndInputExposed injects a prior-run state envelope and a
// user form submission via RunOptions and asserts the handler sees the UNWRAPPED
// state on ctx.state and the submission on ctx.input (#512).
func TestExecute_ResumeStateAndInputExposed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/resumer", `
async def main():
    return {"st": ctx.state, "inp": ctx.input}
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
		Resumed:     true,
		ResumeState: json.RawMessage(`{"__step":null,"state":{"step":"ask_framework","name":"proj"}}`),
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
		t.Fatalf("ctx.state not exposed (unwrapped): %#v", ret["st"])
	}
	if st["step"] != "ask_framework" || st["name"] != "proj" {
		t.Errorf("ctx.state = %#v, want step=ask_framework name=proj", st)
	}
	inp, ok := ret["inp"].(map[string]any)
	if !ok {
		t.Fatalf("ctx.input not exposed: %#v", ret["inp"])
	}
	if inp["project_name"] != "acme" {
		t.Errorf("ctx.input = %#v, want project_name=acme", inp)
	}
}

// TestExecute_SchemalessSuspendAndResume guards the schema-less suspend path
// (#517): a task that calls dicode.suspend(state=...) with NO schema must suspend
// cleanly (the Python SDK sends no `schema` field, so the daemon records no
// constraint) rather than failing with "invalid suspend schema", and a resume
// with input must then succeed.
func TestExecute_SchemalessSuspendAndResume(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/schemaless", `
async def main():
    if ctx.state is None:
        dicode.suspend(state={"step": "ask"})
    return {"resumed": ctx.state is not None, "name": ctx.input.get("project_name")}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	first, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute (suspend): %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID)
	if first.Error != nil {
		t.Fatalf("schema-less suspend must not fail, got error: %v\nlogs:\n%s", first.Error, joinLogs(logs))
	}
	if !first.Suspended {
		t.Fatalf("schema-less suspend must suspend the run\nlogs:\n%s", joinLogs(logs))
	}
	if len(first.ResumeSchema) != 0 {
		t.Errorf("schema-less suspend must record no schema, got %q", first.ResumeSchema)
	}

	runID2, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID:       runID2,
		Resumed:     true,
		ResumeState: json.RawMessage(first.ResumeState),
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Execute (resume): %v", err)
	}
	logs2, _ := reg.GetRunLogs(ctx, runID2)
	if second.Error != nil {
		t.Fatalf("schema-less resume must succeed, got error: %v\nlogs:\n%s", second.Error, joinLogs(logs2))
	}
	if second.Suspended {
		t.Fatalf("resume must not re-suspend\nlogs:\n%s", joinLogs(logs2))
	}
	ret, ok := second.ChainInput.(map[string]any)
	if !ok {
		t.Fatalf("ChainInput = %#v, want map\nlogs:\n%s", second.ChainInput, joinLogs(logs2))
	}
	if ret["resumed"] != true {
		t.Errorf("ctx.state must be set on resume, got resumed=%#v", ret["resumed"])
	}
	if ret["name"] != "acme" {
		t.Errorf("ctx.input[project_name] = %#v, want acme", ret["name"])
	}
}

// TestExecute_ResumeWithNullStateDispatchesResume guards the resume-detection
// fix (#517): a resume whose carried author state is genuinely None must still be
// treated as a resume and dispatch the resume handler with the validated input —
// not fall through to main as a fresh run. The signal is the explicit `resumed`
// flag, never the presence of state.
func TestExecute_ResumeWithNullStateDispatchesResume(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/nullstate", `
async def main():
    return {"via": "main"}

async def resume(ctx):
    return {"via": "resume", "state_none": ctx.state is None, "name": ctx.input["project_name"]}
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
		Resumed:     true,
		ResumeState: nil, // genuinely-null carried state
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID)
	if res.Error != nil {
		t.Fatalf("run error: %v\nlogs:\n%s", res.Error, joinLogs(logs))
	}
	ret, ok := res.ChainInput.(map[string]any)
	if !ok {
		t.Fatalf("ChainInput = %#v, want map\nlogs:\n%s", res.ChainInput, joinLogs(logs))
	}
	if ret["via"] != "resume" {
		t.Errorf("dispatch went to %#v, want the resume handler (not a fresh main)", ret["via"])
	}
	if ret["state_none"] != true {
		t.Errorf("resume must see the None carried state, got state_none=%#v", ret["state_none"])
	}
	if ret["name"] != "acme" {
		t.Errorf("validated submission dropped: ctx.input[project_name] = %#v, want acme", ret["name"])
	}
}

// TestExecute_ResumeMissingStepFailsLoudly guards the missing-marker fix (#517):
// when `steps` is exported but the resume marker names no matching step, the run
// must FAIL loudly rather than silently re-run main/resume against mid-wizard
// state.
func TestExecute_ResumeMissingStepFailsLoudly(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/missingstep", `
async def main():
    return {"via": "main"}

async def known(ctx):
    return {"via": "known"}

steps = {"known": known}
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
		Resumed:     true,
		ResumeState: json.RawMessage(`{"__step":"gone","state":{}}`),
		ResumeInput: json.RawMessage(`{"x":"y"}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	logs, _ := reg.GetRunLogs(ctx, runID)
	if res.Error == nil {
		t.Fatalf("resume to a missing step must fail the run, got no error\nlogs:\n%s", joinLogs(logs))
	}
	if res.ChainInput != nil {
		t.Errorf("resume to a missing step must not run main, got return %#v", res.ChainInput)
	}
	if res.Suspended {
		t.Errorf("resume to a missing step must not be classified as suspended")
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, "is not an exported step function") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a clear diagnostic naming the missing step, logs:\n%s", joinLogs(logs))
	}
}

// TestExecute_NoResumeIsNone verifies that a first (non-resume) invocation sees
// ctx.state as None (#512), so a handler can branch on its presence.
func TestExecute_NoResumeIsNone(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/firstrun", `
async def main():
    return {"has_state": ctx.state is not None}
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
	if ret["has_state"] != false {
		t.Errorf("expected ctx.state None on first run, got %#v", ret)
	}
}
