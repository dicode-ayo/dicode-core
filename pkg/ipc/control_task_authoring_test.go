package ipc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// mockAuthoring is a controllable AuthoringService for the cli.task.*
// handler tests. Each method either returns the canned result or the
// canned error so a test can drive both the happy and failure paths.
type mockAuthoring struct {
	createResult AuthoringCreateResult
	createErr    error
	editResult   AuthoringEditResult
	editErr      error
	saveErr      error
	cancelErr    error
	baseURL      string

	lastCreateName, lastCreateSource string
	lastEditSession, lastEditTask    string
	lastSaveSession, lastCancelSess  string
}

func (m *mockAuthoring) CreateTask(_ context.Context, name, source string) (AuthoringCreateResult, error) {
	m.lastCreateName, m.lastCreateSource = name, source
	return m.createResult, m.createErr
}

func (m *mockAuthoring) EditTask(_ context.Context, sessionID, taskID string) (AuthoringEditResult, error) {
	m.lastEditSession, m.lastEditTask = sessionID, taskID
	return m.editResult, m.editErr
}

func (m *mockAuthoring) SaveTask(_ context.Context, sessionID string) error {
	m.lastSaveSession = sessionID
	return m.saveErr
}

func (m *mockAuthoring) CancelTask(_ context.Context, sessionID string) error {
	m.lastCancelSess = sessionID
	return m.cancelErr
}

func (m *mockAuthoring) WebUIBaseURL() string {
	if m.baseURL == "" {
		return "http://localhost:8080"
	}
	return m.baseURL
}

func newAuthoringControl(a AuthoringService) *ControlServer {
	return &ControlServer{authoring: a, log: zap.NewNop()}
}

func TestControl_TaskCreate_NoService(t *testing.T) {
	cs := &ControlServer{log: zap.NewNop()}
	_, err := cs.handleTaskCreate(context.Background(), Request{TaskName: "x"})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want 'not configured'", err)
	}
}

func TestControl_TaskCreate_Plain(t *testing.T) {
	m := &mockAuthoring{createResult: AuthoringCreateResult{
		TaskID: "ai-scratch/hello", Source: "ai-scratch", Files: []string{"task.yaml", "task.js"},
	}}
	cs := newAuthoringControl(m)
	res, err := cs.handleTaskCreate(context.Background(), Request{TaskName: "hello", Source: "ai-scratch"})
	if err != nil {
		t.Fatalf("handleTaskCreate: %v", err)
	}
	if res.TaskID != "ai-scratch/hello" || res.Source != "ai-scratch" {
		t.Errorf("res = %+v", res)
	}
	if res.SessionID != "" || res.WebUIURL != "" {
		t.Errorf("plain create must not carry edit metadata: %+v", res)
	}
	if m.lastCreateName != "hello" || m.lastCreateSource != "ai-scratch" {
		t.Errorf("service args: name=%q source=%q", m.lastCreateName, m.lastCreateSource)
	}
}

func TestControl_TaskCreate_WithAIChainsEdit(t *testing.T) {
	m := &mockAuthoring{
		createResult: AuthoringCreateResult{TaskID: "ai-scratch/hello", Source: "ai-scratch"},
		editResult:   AuthoringEditResult{SessionID: "sess-1", Source: "ai-scratch", SourceKind: "local"},
		baseURL:      "http://localhost:9999",
	}
	cs := newAuthoringControl(m)
	res, err := cs.handleTaskCreate(context.Background(), Request{TaskName: "hello", Prompt: "do a thing"})
	if err != nil {
		t.Fatalf("handleTaskCreate: %v", err)
	}
	if res.SessionID != "sess-1" {
		t.Errorf("session id = %q, want sess-1", res.SessionID)
	}
	if !strings.Contains(res.WebUIURL, "sess-1") || !strings.HasPrefix(res.WebUIURL, "http://localhost:9999") {
		t.Errorf("webui url = %q", res.WebUIURL)
	}
	if m.lastEditTask != "ai-scratch/hello" {
		t.Errorf("edit chained with task %q, want ai-scratch/hello", m.lastEditTask)
	}
}

func TestControl_TaskCreate_AIEditFailureKeepsTaskID(t *testing.T) {
	m := &mockAuthoring{
		createResult: AuthoringCreateResult{TaskID: "ai-scratch/hello"},
		editErr:      errors.New("boom"),
	}
	cs := newAuthoringControl(m)
	res, err := cs.handleTaskCreate(context.Background(), Request{TaskName: "hello", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "edit session failed") {
		t.Fatalf("err = %v, want edit-failure", err)
	}
	if res.TaskID != "ai-scratch/hello" {
		t.Errorf("task id should survive edit failure, got %q", res.TaskID)
	}
}

func TestControl_TaskEdit_BuildsWebUIURL(t *testing.T) {
	m := &mockAuthoring{
		editResult: AuthoringEditResult{SessionID: "abc", TaskID: "ai-scratch/t", Source: "ai-scratch", SourceKind: "local"},
		baseURL:    "https://host:1234",
	}
	cs := newAuthoringControl(m)
	res, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "p"})
	if err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	// TaskID echoes the service result (sess.TaskID), not the request's claim.
	if res.SessionID != "abc" || res.TaskID != "ai-scratch/t" {
		t.Errorf("res = %+v", res)
	}
	if res.WebUIURL != "https://host:1234/?session=abc" {
		t.Errorf("webui url = %q", res.WebUIURL)
	}
}

func TestControl_TaskEdit_ConflictSurfaces(t *testing.T) {
	m := &mockAuthoring{editErr: errors.New(`source "ai-scratch" already has an open session s2 for task "ai-scratch/other" (#283)`)}
	cs := newAuthoringControl(m)
	_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "#283") {
		t.Fatalf("conflict err = %v, want #283 mention", err)
	}
}

func TestControl_TaskSave(t *testing.T) {
	m := &mockAuthoring{}
	cs := newAuthoringControl(m)
	res, err := cs.handleTaskSave(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("handleTaskSave: %v", err)
	}
	if !res.Applied || res.SessionID != "s1" {
		t.Errorf("res = %+v", res)
	}
	if m.lastSaveSession != "s1" {
		t.Errorf("save session = %q", m.lastSaveSession)
	}
}

func TestControl_TaskSave_Error(t *testing.T) {
	m := &mockAuthoring{saveErr: errors.New("session not found")}
	cs := newAuthoringControl(m)
	_, err := cs.handleTaskSave(context.Background(), Request{SessionID: "missing"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestControl_TaskCancel(t *testing.T) {
	m := &mockAuthoring{}
	cs := newAuthoringControl(m)
	res, err := cs.handleTaskCancel(context.Background(), Request{SessionID: "s9"})
	if err != nil {
		t.Fatalf("handleTaskCancel: %v", err)
	}
	if !res.Cancelled || res.SessionID != "s9" {
		t.Errorf("res = %+v", res)
	}
	if m.lastCancelSess != "s9" {
		t.Errorf("cancel session = %q", m.lastCancelSess)
	}
}
