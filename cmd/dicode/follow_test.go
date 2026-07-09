package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/ipc"
)

// fakeFollowClient scripts the daemon responses the follow loop consumes. Each
// suspended run id maps to the schema it awaits; resume returns the next id from
// resumeChain; waitResults maps a continuation id to its settled RunResult.
type fakeFollowClient struct {
	schemas     map[string][]byte          // runID -> resume JSON Schema
	resumeChain map[string]string          // suspended runID -> continuation runID
	waitResults map[string]ipc.RunResult   // continuation runID -> settled result
	logs        map[string][]ipc.LogEntry  // runID -> log lines
	submitted   map[string]json.RawMessage // suspended runID -> input submitted
}

func (f *fakeFollowClient) ResumeGet(runID string) (ipc.ResumeInfo, error) {
	return ipc.ResumeInfo{RunID: runID, Schema: f.schemas[runID]}, nil
}

func (f *fakeFollowClient) Resume(runID string, input []byte) (ipc.ResumeResult, error) {
	if f.submitted == nil {
		f.submitted = map[string]json.RawMessage{}
	}
	f.submitted[runID] = append(json.RawMessage(nil), input...)
	return ipc.ResumeResult{RunID: f.resumeChain[runID]}, nil
}

func (f *fakeFollowClient) WaitRun(runID string) (ipc.RunResult, error) {
	return f.waitResults[runID], nil
}

func (f *fakeFollowClient) Logs(runID string) ([]ipc.LogEntry, error) {
	return f.logs[runID], nil
}

const oneStepSchema = `{"type":"object","properties":{"approve":{"type":"string","title":"Approve?"}},"required":["approve"]}`

func TestFollow_SingleStepToSuccess(t *testing.T) {
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(oneStepSchema)},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{
			"run-2": {RunID: "run-2", Status: "success", ReturnValue: map[string]any{"ok": true}},
		},
		logs: map[string][]ipc.LogEntry{
			"run-2": {{Timestamp: "t0", Level: "info", Message: "done"}},
		},
	}
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader("yes\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}

	// The typed answer was submitted for the suspended run.
	if got := string(client.submitted["run-1"]); got != `{"approve":"yes"}` {
		t.Errorf("submitted = %s, want {\"approve\":\"yes\"}", got)
	}
	stdout := out.String()
	if !strings.Contains(stdout, "run run-2: success") {
		t.Errorf("stdout missing final status:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"ok": true`) {
		t.Errorf("stdout missing return value:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[info] done") {
		t.Errorf("stdout missing streamed logs:\n%s", stdout)
	}
}

func TestFollow_MultiStepWizard(t *testing.T) {
	client := &fakeFollowClient{
		schemas: map[string][]byte{
			"run-1": []byte(oneStepSchema),
			"run-2": []byte(`{"type":"object","properties":{"env":{"type":"string","enum":["staging","prod"]}},"required":["env"]}`),
		},
		resumeChain: map[string]string{"run-1": "run-2", "run-2": "run-3"},
		waitResults: map[string]ipc.RunResult{
			"run-2": {RunID: "run-2", Status: "suspended"},
			"run-3": {RunID: "run-3", Status: "success"},
		},
	}
	var out, prompt bytes.Buffer
	// First step: approve=go, second step: pick enum value prod.
	s := &followSession{client: client, in: strings.NewReader("go\nprod\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if got := string(client.submitted["run-2"]); got != `{"env":"prod"}` {
		t.Errorf("step-2 submitted = %s, want {\"env\":\"prod\"}", got)
	}
	if !strings.Contains(prompt.String(), "suspended again") {
		t.Errorf("expected re-prompt banner for the second step:\n%s", prompt.String())
	}
	if !strings.Contains(out.String(), "run run-3: success") {
		t.Errorf("expected terminal success on run-3:\n%s", out.String())
	}
}

func TestFollow_FailureReturnsError(t *testing.T) {
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(oneStepSchema)},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{"run-2": {RunID: "run-2", Status: "failure"}},
	}
	var out, prompt bytes.Buffer
	s := &followSession{client: client, in: strings.NewReader("yes\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err == nil {
		t.Fatal("expected error when the continuation fails")
	}
	if !strings.Contains(out.String(), "run run-2: failure") {
		t.Errorf("stdout should still report the failing status:\n%s", out.String())
	}
}

func TestFollow_RepromptsUntilRequiredSupplied(t *testing.T) {
	client := &fakeFollowClient{
		schemas:     map[string][]byte{"run-1": []byte(oneStepSchema)},
		resumeChain: map[string]string{"run-1": "run-2"},
		waitResults: map[string]ipc.RunResult{"run-2": {RunID: "run-2", Status: "success"}},
	}
	var out, prompt bytes.Buffer
	// Blank line first (required -> reprompt), then a real value.
	s := &followSession{client: client, in: strings.NewReader("\nyes\n"), prompt: &prompt, out: &out}

	if err := s.follow("run-1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if !strings.Contains(prompt.String(), "required — please enter a value") {
		t.Errorf("expected required-field reprompt:\n%s", prompt.String())
	}
	if got := string(client.submitted["run-1"]); got != `{"approve":"yes"}` {
		t.Errorf("submitted = %s, want the value entered after the reprompt", got)
	}
}
