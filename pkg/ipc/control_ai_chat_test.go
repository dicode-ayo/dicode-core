package ipc

import (
	"context"
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
