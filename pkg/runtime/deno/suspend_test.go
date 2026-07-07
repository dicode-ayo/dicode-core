package deno

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRun_SuspendYieldsSuspendedResult runs a real Deno task that calls
// dicode.suspend({ state, form, deadline }) and asserts the run ends as a
// clean suspend (not a failure) with the state/form/deadline captured on the
// RunResult (#95). The SuspendSignal thrown by the SDK must exit the process
// with code 0.
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
    form: {
      title: "Your name?",
      fields: [{ name: "project_name", type: "string", label: "Name", required: true }],
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

	var form struct {
		Title  string `json:"title"`
		Fields []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(res.ResumeForm, &form); err != nil {
		t.Fatalf("ResumeForm not valid JSON (%q): %v", res.ResumeForm, err)
	}
	if form.Title != "Your name?" || len(form.Fields) != 1 || form.Fields[0].Name != "project_name" {
		t.Errorf("ResumeForm = %+v, want title + one project_name field", form)
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
      form: { fields: [{ name: "x", type: "string", label: "X" }] },
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

// TestRun_ResumeStateAndInputExposed injects a prior-run state blob and a
// user form submission via RunOptions and asserts the task sees them on
// ctx.resume_state / ctx.resume_input (#95). Nothing in PR 2 populates these
// from a real suspended run yet — this verifies the accept + expose mechanism
// with an injected value.
func TestRun_ResumeStateAndInputExposed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "resumer", `
export default async function main({ resume_state, resume_input }) {
  return { st: resume_state, inp: resume_input };
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	res, err := rt.Run(ctx, spec, RunOptions{
		RunID:       "run-resume-1",
		ResumeState: json.RawMessage(`{"step":"ask_framework","name":"proj"}`),
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

// TestRun_NoResumeIsUndefined verifies that a first (non-resume) invocation
// sees ctx.resume_state / ctx.resume_input as undefined (#95), so a task can
// branch on their presence.
func TestRun_NoResumeIsUndefined(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "firstrun", `
export default async function main({ resume_state, resume_input }) {
  return { hasState: resume_state !== undefined, hasInput: resume_input !== undefined };
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
	if ret["hasState"] != false || ret["hasInput"] != false {
		t.Errorf("expected resume_state/resume_input undefined on first run, got %#v", ret)
	}
}
