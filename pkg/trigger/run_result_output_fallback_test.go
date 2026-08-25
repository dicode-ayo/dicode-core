package trigger

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/registry"
)

// A task that fails via the shared ai-agent terminal-failure pattern
// (output.json(envelope); throw — see tasks/buildin/ai-agent-core/chat.ts,
// fixed by #750) never bare-returns: return_value stays empty even though the
// caller-facing envelope was published as structured output. buildRunResult
// must fall back to OutputContent so a `dicode ai`/control-socket caller (and
// dicode.run_task callers reading ipc.RunResult.ReturnValue) see the same
// detail a webhook caller already gets over HTTP 500 (pkg/trigger/webhook.go).
func TestBuildRunResult_FallsBackToJSONOutputContent_WhenNoReturnValue(t *testing.T) {
	eng, reg := waitEnv(t)
	ctx := context.Background()

	runID, err := reg.StartRun(ctx, "buildin/ai-agent", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	envelope := `{"error":"not_configured","reply":"not configured — missing model, base_url.","missing":["model","base_url"]}`
	if err := reg.FinishRunWithResult(ctx, runID, registry.StatusFailure, "" /* no bare return */, "application/json", envelope); err != nil {
		t.Fatalf("FinishRunWithResult: %v", err)
	}

	res, err := eng.WaitRunSettled(ctx, runID)
	if err != nil {
		t.Fatalf("WaitRunSettled: %v", err)
	}
	if res.Status != registry.StatusFailure {
		t.Fatalf("Status = %q, want %q", res.Status, registry.StatusFailure)
	}
	m, ok := res.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("ReturnValue = %#v (%T), want the OutputContent envelope unmarshalled as map[string]any", res.ReturnValue, res.ReturnValue)
	}
	if m["error"] != "not_configured" {
		t.Errorf(`ReturnValue["error"] = %v, want "not_configured"`, m["error"])
	}
	if m["reply"] != "not configured — missing model, base_url." {
		t.Errorf(`ReturnValue["reply"] = %v, want the envelope's reply`, m["reply"])
	}
}

// A non-JSON structured output (output.html()/output.text()/output.image())
// must never be misparsed as the return value — only application/json falls
// back.
func TestBuildRunResult_DoesNotFallBackToNonJSONOutputContent(t *testing.T) {
	eng, reg := waitEnv(t)
	ctx := context.Background()

	runID, err := reg.StartRun(ctx, "some/task", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := reg.FinishRunWithResult(ctx, runID, registry.StatusFailure, "", "text/html", "<h1>oops</h1>"); err != nil {
		t.Fatalf("FinishRunWithResult: %v", err)
	}

	res, err := eng.WaitRunSettled(ctx, runID)
	if err != nil {
		t.Fatalf("WaitRunSettled: %v", err)
	}
	if res.ReturnValue != nil {
		t.Errorf("ReturnValue = %#v, want nil for a text/html output (must not be parsed as JSON)", res.ReturnValue)
	}
}

// A bare return value takes precedence over OutputContent — the fallback only
// kicks in when there is genuinely nothing else.
func TestBuildRunResult_BareReturnValueTakesPrecedenceOverOutputContent(t *testing.T) {
	eng, reg := waitEnv(t)
	ctx := context.Background()

	runID, err := reg.StartRun(ctx, "some/task", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := reg.FinishRunWithResult(ctx, runID, registry.StatusSuccess, `{"reply":"from return"}`, "application/json", `{"reply":"from output.json"}`); err != nil {
		t.Fatalf("FinishRunWithResult: %v", err)
	}

	res, err := eng.WaitRunSettled(ctx, runID)
	if err != nil {
		t.Fatalf("WaitRunSettled: %v", err)
	}
	m, ok := res.ReturnValue.(map[string]any)
	if !ok || m["reply"] != "from return" {
		t.Errorf("ReturnValue = %#v, want the bare return value to win", res.ReturnValue)
	}
}

// The OutputContent fallback is scoped to a non-successful run. A successful
// run that calls output.json() purely for display (e.g. a report task) and
// bare-returns nothing must keep getting a nil ReturnValue, exactly like
// before this fix — widening what counts as "the return value" for every
// other task's dicode.run_task()/pipeline-chain callers on success would be
// an untested, surprising behavior change unrelated to the failed-run bug
// this fallback exists for.
func TestBuildRunResult_SuccessfulRun_DoesNotFallBackToOutputContent(t *testing.T) {
	eng, reg := waitEnv(t)
	ctx := context.Background()

	runID, err := reg.StartRun(ctx, "some/report-task", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := reg.FinishRunWithResult(ctx, runID, registry.StatusSuccess, "" /* no bare return */, "application/json", `{"display":"only"}`); err != nil {
		t.Fatalf("FinishRunWithResult: %v", err)
	}

	res, err := eng.WaitRunSettled(ctx, runID)
	if err != nil {
		t.Fatalf("WaitRunSettled: %v", err)
	}
	if res.ReturnValue != nil {
		t.Errorf("ReturnValue = %#v, want nil — a successful run's display-only output.json() must not become its return value", res.ReturnValue)
	}
}
