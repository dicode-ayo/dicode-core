package deno

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRun_SuspendYieldsSuspendedResult runs a real Deno task that calls
// dicode.suspend({ state, schema, deadline }) and asserts the run ends as a
// clean suspend (not a failure) with the state/schema/deadline captured on the
// RunResult (#512). The runner persists `state` wrapped in a { __step, state }
// envelope so it can dispatch handlers on resume; the author's blob rides in
// `state`. The SuspendSignal thrown by the SDK must exit the process with 0.
func TestRun_SuspendYieldsSuspendedResult(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "wizard", `
export default async function main({ dicode }) {
  await dicode.suspend({
    state: { step: "ask_name", n: 42 },
    schema: {
      type: "object",
      title: "Your name?",
      properties: { project_name: { type: "string", title: "Name" } },
      required: ["project_name"],
    },
    deadline: 1893456000000,
  });
  // Unreachable: suspend never resolves.
  return { reached: true };
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	res, err := rt.Run(ctx, spec, RunOptions{RunID: "run-suspend-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("suspend must not be a failure, got error: %v", res.Error)
	}
	if !res.Suspended {
		t.Fatalf("expected Suspended = true, got false")
	}
	if res.ReturnValue != nil {
		t.Errorf("suspend must not produce a return value, got %#v", res.ReturnValue)
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

// TestRun_MainOnlyResumeReRunsMain is the single-main back-compat guard: a task
// exporting only `main` (no `resume`, no `steps`) re-runs `main` on resume, and
// its author state round-trips through the __step envelope invisibly — the first
// run sees ctx.state undefined, the resume sees the real unwrapped blob so the
// author can branch by hand (#512).
func TestRun_MainOnlyResumeReRunsMain(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "loopguard", `
export default async function main({ dicode, state, input }) {
  if (!state) {
    await dicode.suspend({
      state: { step: "ask_name" },
      schema: { type: "object", properties: { project_name: { type: "string" } }, required: ["project_name"] },
    });
  }
  return { defined: state !== undefined, step: (state as any).step, name: (input as any).project_name };
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	first, err := rt.Run(ctx, spec, RunOptions{RunID: "run-loopguard-1"})
	if err != nil {
		t.Fatalf("Run (suspend): %v", err)
	}
	if !first.Suspended || len(first.ResumeState) == 0 {
		t.Fatalf("first run must suspend and capture a state blob; got suspended=%v state=%q", first.Suspended, first.ResumeState)
	}

	// Replay the stored envelope exactly as the resume path would.
	second, err := rt.Run(ctx, spec, RunOptions{
		RunID:       "run-loopguard-2",
		Resumed:     true,
		ResumeState: json.RawMessage(first.ResumeState),
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Run (resume): %v", err)
	}
	if second.Suspended {
		t.Fatalf("resume must not re-suspend — the state guard failed to flip")
	}
	ret, ok := second.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("ReturnValue = %#v, want map", second.ReturnValue)
	}
	if ret["defined"] != true {
		t.Errorf("ctx.state must be defined on resume, got %#v", ret["defined"])
	}
	if ret["step"] != "ask_name" {
		t.Errorf("ctx.state.step = %#v, want ask_name", ret["step"])
	}
	if ret["name"] != "acme" {
		t.Errorf("ctx.input.project_name = %#v, want acme", ret["name"])
	}
}

// TestRun_ResumeDispatchesToResumeFn covers the two-function shape: a task
// exports `main` (first run) and `resume` (resume). The runner must run `resume`
// on the continuation, handing it the carried state and the validated input via
// ctx.state / ctx.input (#512).
func TestRun_ResumeDispatchesToResumeFn(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "twofunc", `
export default async function main({ dicode }) {
  await dicode.suspend({
    state: { greeting: "hi" },
    schema: { type: "object", properties: { project_name: { type: "string" } }, required: ["project_name"] },
  });
  return { via: "main" };
}
export async function resume({ state, input }) {
  return { via: "resume", greeting: (state as any).greeting, name: (input as any).project_name };
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	first, err := rt.Run(ctx, spec, RunOptions{RunID: "run-twofunc-1"})
	if err != nil {
		t.Fatalf("Run (suspend): %v", err)
	}
	if !first.Suspended {
		t.Fatalf("first run must suspend")
	}

	second, err := rt.Run(ctx, spec, RunOptions{
		RunID:       "run-twofunc-2",
		Resumed:     true,
		ResumeState: json.RawMessage(first.ResumeState),
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Run (resume): %v", err)
	}
	if second.Suspended {
		t.Fatalf("resume must not re-suspend")
	}
	ret, ok := second.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("ReturnValue = %#v, want map", second.ReturnValue)
	}
	if ret["via"] != "resume" {
		t.Errorf("dispatch went to %#v, want the resume handler", ret["via"])
	}
	if ret["greeting"] != "hi" {
		t.Errorf("ctx.state.greeting = %#v, want hi", ret["greeting"])
	}
	if ret["name"] != "acme" {
		t.Errorf("ctx.input.project_name = %#v, want acme", ret["name"])
	}
}

// TestRun_StepsWizardMultiStep drives a full named-step wizard (#512): main
// suspends `to: "chooseFramework"`, that step sees ctx.input and suspends
// `to: "deploy"` carrying state, and deploy sees the prior state + the new input
// and returns. Exercises __step envelope threading across two hops, and asserts
// main sees ctx.state undefined on the first run even with the internal marker.
func TestRun_StepsWizardMultiStep(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "steps-wizard", `
export default async function main({ dicode, state }) {
  if (state !== undefined) throw new Error("main must see ctx.state undefined on the first run");
  await dicode.suspend({
    to: "chooseFramework",
    schema: { type: "object", properties: { framework: { type: "string" } }, required: ["framework"] },
  });
  return { via: "main" };
}
export const steps = {
  async chooseFramework({ dicode, input }) {
    await dicode.suspend({
      to: "deploy",
      state: { framework: (input as any).framework },
      schema: { type: "object", properties: { env: { type: "string" } }, required: ["env"] },
    });
    return { via: "chooseFramework" };
  },
  async deploy({ state, input }) {
    return { framework: (state as any).framework, env: (input as any).env };
  },
};
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	// Run 1 — main suspends to chooseFramework.
	r1, err := rt.Run(ctx, spec, RunOptions{RunID: "run-steps-1"})
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if !r1.Suspended {
		t.Fatalf("run 1 must suspend")
	}
	var env1 struct {
		Step string `json:"__step"`
	}
	if err := json.Unmarshal(r1.ResumeState, &env1); err != nil {
		t.Fatalf("run 1 ResumeState: %v", err)
	}
	if env1.Step != "chooseFramework" {
		t.Fatalf("__step = %q, want chooseFramework", env1.Step)
	}

	// Run 2 — chooseFramework sees the framework input, suspends to deploy.
	r2, err := rt.Run(ctx, spec, RunOptions{
		RunID:       "run-steps-2",
		Resumed:     true,
		ResumeState: json.RawMessage(r1.ResumeState),
		ResumeInput: json.RawMessage(`{"framework":"deno"}`),
	})
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if !r2.Suspended {
		t.Fatalf("run 2 must suspend again")
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
	r3, err := rt.Run(ctx, spec, RunOptions{
		RunID:       "run-steps-3",
		Resumed:     true,
		ResumeState: json.RawMessage(r2.ResumeState),
		ResumeInput: json.RawMessage(`{"env":"prod"}`),
	})
	if err != nil {
		t.Fatalf("Run 3: %v", err)
	}
	if r3.Suspended {
		t.Fatalf("run 3 must finish, not suspend")
	}
	ret, ok := r3.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("ReturnValue = %#v, want map", r3.ReturnValue)
	}
	if ret["framework"] != "deno" {
		t.Errorf("deploy ctx.state.framework = %#v, want deno", ret["framework"])
	}
	if ret["env"] != "prod" {
		t.Errorf("deploy ctx.input.env = %#v, want prod", ret["env"])
	}
}

// TestRun_SuspendSwallowedByTaskFails guards the case where a task wraps its
// body in a broad try/catch that swallows the SuspendSignal thrown by
// dicode.suspend() and keeps executing (#95). The suspend payload is already
// recorded server-side, so a normal return would leave the run in a
// contradictory suspended-and-returned state. The run must instead FAIL
// loudly and NOT be classified as suspended.
func TestRun_SuspendSwallowedByTaskFails(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "swallower", `
export default async function main({ dicode }) {
  try {
    await dicode.suspend({
      state: { step: "one" },
      schema: { type: "object", properties: { x: { type: "string" } } },
    });
  } catch (_e) {
    // Swallow the control-flow signal and keep going — the bug we guard against.
  }
  return { kept_running: true };
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	res, err := rt.Run(ctx, spec, RunOptions{RunID: "run-swallow-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Suspended {
		t.Fatalf("swallowed suspend must not be classified as suspended")
	}
	if res.Error == nil {
		t.Fatalf("swallowed suspend must fail the run, got no error")
	}
	if res.ReturnValue != nil {
		t.Errorf("swallowed suspend must not record a return value, got %#v", res.ReturnValue)
	}

	logs, _ := reg.GetRunLogs(ctx, "run-swallow-1")
	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, "control-flow signal was caught by task code") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a clear diagnostic in the run log, logs = %+v", logs)
	}
}

// TestRun_ResumeStateAndInputExposed injects a prior-run state envelope and a
// user form submission via RunOptions and asserts the handler sees the UNWRAPPED
// state on ctx.state and the submission on ctx.input (#512).
func TestRun_ResumeStateAndInputExposed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "resumer", `
export default async function main({ state, input }) {
  return { st: state, inp: input };
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	res, err := rt.Run(ctx, spec, RunOptions{
		RunID:       "run-resume-1",
		Resumed:     true,
		ResumeState: json.RawMessage(`{"__step":null,"state":{"step":"ask_framework","name":"proj"}}`),
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("run error: %v", res.Error)
	}
	if res.Suspended {
		t.Fatalf("resume run must not be suspended")
	}

	ret, ok := res.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("ReturnValue = %#v, want map", res.ReturnValue)
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

// TestRun_ResumeWithNullStateDispatchesResume guards the resume-detection fix
// (#517): a resume whose carried author state is genuinely null must still be
// treated as a resume and dispatch the resume handler with the validated input —
// not fall through to main as a fresh run and drop the submission. The signal is
// the explicit `resumed` flag, never the presence of state.
func TestRun_ResumeWithNullStateDispatchesResume(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "nullstate", `
export default async function main() {
  return { via: "main" };
}
export async function resume({ state, input }) {
  return { via: "resume", stateNull: state === null || state === undefined, name: (input as any).project_name };
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	res, err := rt.Run(ctx, spec, RunOptions{
		RunID:       "run-nullstate-1",
		Resumed:     true,
		ResumeState: nil, // genuinely-null carried state
		ResumeInput: json.RawMessage(`{"project_name":"acme"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("run error: %v", res.Error)
	}
	ret, ok := res.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("ReturnValue = %#v, want map", res.ReturnValue)
	}
	if ret["via"] != "resume" {
		t.Errorf("dispatch went to %#v, want the resume handler (not a fresh main)", ret["via"])
	}
	if ret["stateNull"] != true {
		t.Errorf("resume must see the null carried state, got stateNull=%#v", ret["stateNull"])
	}
	if ret["name"] != "acme" {
		t.Errorf("validated submission dropped: ctx.input.project_name = %#v, want acme", ret["name"])
	}
}

// TestRun_ResumeMissingStepFailsLoudly guards the missing-marker fix (#517): when
// `steps` is exported but the resume marker names no matching step (typo, or the
// task was edited mid-wizard), the run must FAIL loudly rather than silently
// re-run main/resume against mid-wizard state.
func TestRun_ResumeMissingStepFailsLoudly(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "missingstep", `
export default async function main() {
  return { via: "main" };
}
export const steps = {
  async known() { return { via: "known" }; },
};
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	res, err := rt.Run(ctx, spec, RunOptions{
		RunID:       "run-missingstep-1",
		Resumed:     true,
		ResumeState: json.RawMessage(`{"__step":"gone","state":{}}`),
		ResumeInput: json.RawMessage(`{"x":"y"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Error == nil {
		t.Fatalf("resume to a missing step must fail the run, got no error")
	}
	if res.ReturnValue != nil {
		t.Errorf("resume to a missing step must not run main, got return %#v", res.ReturnValue)
	}
	if res.Suspended {
		t.Errorf("resume to a missing step must not be classified as suspended")
	}

	logs, _ := reg.GetRunLogs(ctx, "run-missingstep-1")
	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, "is not an exported step function") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a clear diagnostic naming the missing step, logs = %+v", logs)
	}
}

// TestRun_NoResumeIsUndefined verifies that a first (non-resume) invocation sees
// ctx.state undefined (#512), so a handler can branch on its presence. ctx.input
// on a first run is the trigger input (nil here), not the resume submission.
func TestRun_NoResumeIsUndefined(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "firstrun", `
export default async function main({ state }) {
  return { hasState: state !== undefined };
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	res, err := rt.Run(ctx, spec, RunOptions{RunID: "run-first-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("run error: %v", res.Error)
	}
	ret, ok := res.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("ReturnValue = %#v, want map", res.ReturnValue)
	}
	if ret["hasState"] != false {
		t.Errorf("expected ctx.state undefined on first run, got %#v", ret)
	}
}
