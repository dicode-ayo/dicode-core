package ipc

import (
	"context"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// chatSuspendEngine simulates the agent task suspending on a chat-start run.
type chatSuspendEngine struct {
	mockEngine
	firedParams map[string]string
	usedWaitRun bool
}

func (e *chatSuspendEngine) FireManual(_ context.Context, _ string, params map[string]string) (string, error) {
	e.firedParams = params
	return "run-chat-1", nil
}

func (e *chatSuspendEngine) WaitRunSettled(_ context.Context, runID string) (RunResult, error) {
	return RunResult{RunID: runID, Status: registry.StatusSuspended}, nil
}

// WaitRun following a suspended chat-start run (never resumed here) would block
// forever; handleAI must not call it.
func (e *chatSuspendEngine) WaitRun(_ context.Context, runID string) (RunResult, error) {
	e.usedWaitRun = true
	return RunResult{RunID: runID, Status: registry.StatusSuspended}, nil
}

func newAITestServer(t *testing.T, eng EngineRunner) *ControlServer {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	if err := reg.Register(&task.Spec{
		ID: "buildin/ai-agent", Name: "ai-agent",
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return &ControlServer{engine: eng, reg: reg, defaultAITask: "buildin/ai-agent", log: zap.NewNop()}
}

// TestHandleAI_BlankPrompt_EntersChatSuspend: a blank prompt opens chat mode —
// the run suspends and handleAI surfaces it (Suspended=true + RunID) via the
// settled waiter, without sending an empty `prompt` param and without blocking
// on WaitRun.
func TestHandleAI_BlankPrompt_EntersChatSuspend(t *testing.T) {
	eng := &chatSuspendEngine{}
	cs := newAITestServer(t, eng)

	out, err := cs.handleAI(context.Background(), Request{Method: "cli.ai", Prompt: ""})
	if err != nil {
		t.Fatalf("handleAI blank prompt: %v", err)
	}
	if !out.Suspended {
		t.Error("blank prompt must surface Suspended=true for chat mode")
	}
	if out.RunID != "run-chat-1" {
		t.Errorf("RunID = %q, want run-chat-1", out.RunID)
	}
	if eng.usedWaitRun {
		t.Error("handleAI used WaitRun on a chat-start run — would block until resume; must use WaitRunSettled")
	}
	if _, ok := eng.firedParams["prompt"]; ok {
		t.Error("blank prompt must not be sent as a param — chat-start keys on its absence")
	}
}

// oneShotEngine returns a normal successful run with a reply envelope.
type oneShotEngine struct {
	mockEngine
	firedParams map[string]string
}

func (e *oneShotEngine) FireManual(_ context.Context, _ string, params map[string]string) (string, error) {
	e.firedParams = params
	return "run-1shot", nil
}

func (e *oneShotEngine) WaitRunSettled(_ context.Context, runID string) (RunResult, error) {
	return RunResult{
		RunID:  runID,
		Status: "success",
		ReturnValue: map[string]any{
			"reply":      "hi there",
			"session_id": "sess-9",
		},
	}, nil
}

// TestHandleAI_Prompt_OneShotUnchanged: a prompt still runs a single turn and
// returns the reply/session envelope, not suspended.
func TestHandleAI_Prompt_OneShotUnchanged(t *testing.T) {
	eng := &oneShotEngine{}
	cs := newAITestServer(t, eng)

	out, err := cs.handleAI(context.Background(), Request{Method: "cli.ai", Prompt: "hello"})
	if err != nil {
		t.Fatalf("handleAI: %v", err)
	}
	if out.Suspended {
		t.Error("a prompted one-shot run must not be marked Suspended")
	}
	if out.Reply != "hi there" || out.SessionID != "sess-9" {
		t.Errorf("out = %+v, want reply/session from the envelope", out)
	}
	if eng.firedParams["prompt"] != "hello" {
		t.Errorf("prompt param = %q, want hello", eng.firedParams["prompt"])
	}
}

// failedTurnEngine simulates a run that failed via the ai-agent terminal-failure
// pattern (output.json(envelope); throw — see ai-agent-core/chat.ts): the run
// settles with Status "failure" and ReturnValue populated from the run's
// structured output (buildRunResult's OutputContent fallback, pkg/trigger/run.go),
// not from a bare return.
type failedTurnEngine struct {
	mockEngine
}

func (e *failedTurnEngine) FireManual(_ context.Context, _ string, _ map[string]string) (string, error) {
	return "run-failed-1", nil
}

func (e *failedTurnEngine) WaitRunSettled(_ context.Context, runID string) (RunResult, error) {
	return RunResult{
		RunID:  runID,
		Status: "failure",
		ReturnValue: map[string]any{
			"error":   "not_configured",
			"reply":   "not configured — missing model, base_url. This is the generic ai-agent buildin...",
			"missing": []any{"model", "base_url"},
			"hint":    "This is the generic ai-agent buildin...",
		},
	}, nil
}

// TestHandleAI_FailedRun_SurfacesEnvelopeDetail: a failed run's error must
// carry the run's own reply/hint detail (from ReturnValue), not just the bare
// run id and status — otherwise a `dicode ai` caller against an unconfigured
// provider sees only "finished with status failure — see dicode logs", losing
// exactly the actionable detail (which fields are missing) the fix in #750
// worked to preserve for a webhook caller.
func TestHandleAI_FailedRun_SurfacesEnvelopeDetail(t *testing.T) {
	eng := &failedTurnEngine{}
	cs := newAITestServer(t, eng)

	_, err := cs.handleAI(context.Background(), Request{Method: "cli.ai", Prompt: "hello"})
	if err == nil {
		t.Fatal("handleAI: expected an error for a failed run, got nil")
	}
	if !strings.Contains(err.Error(), "missing model, base_url") {
		t.Errorf("handleAI error = %q, want it to include the envelope's reply detail", err.Error())
	}
	if !strings.Contains(err.Error(), "run-failed-1") {
		t.Errorf("handleAI error = %q, want it to still name the run id", err.Error())
	}
}

// TestRunFailureError_NoEnvelope_FallsBackToGenericMessage: a failed run whose
// ReturnValue isn't a map[string]any (nil, or a task that failed some other
// way with no structured output) must still get the generic status message,
// not a panic or an empty error.
func TestRunFailureError_NoEnvelope_FallsBackToGenericMessage(t *testing.T) {
	err := runFailureError(RunResult{RunID: "run-2", Status: "failure", ReturnValue: nil})
	if err == nil || !strings.Contains(err.Error(), "run-2") || !strings.Contains(err.Error(), "failure") {
		t.Errorf("runFailureError(nil ReturnValue) = %v, want a generic run-id/status message", err)
	}
}
