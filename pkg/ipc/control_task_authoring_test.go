package ipc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// mockAuthoring is a controllable AuthoringService for the cli.task.*
// handler tests. Each method either returns the canned result or the
// canned error so a test can drive both the happy and failure paths.
//
// agentSessionIDs simulates the author_sessions.agent_session_id column
// (#568): EditTask overlays it onto editResult.AgentSessionID by session id,
// and UpdateAgentSessionID writes into it — so a test can drive two
// successive handleTaskEdit calls and observe the agent session id carrying
// across calls (run-group correlation, not conversational memory — see
// handleTaskEdit's doc comment) the same way the real DB-backed
// authoringSessionStore would provide it.
type mockAuthoring struct {
	createResult AuthoringCreateResult
	createErr    error
	editResult   AuthoringEditResult
	editErr      error
	saveErr      error
	cancelErr    error
	baseURL      string

	// mu guards every field below. The concurrency tests in this file drive
	// several goroutines through handleTaskEdit against one mockAuthoring;
	// EditTask/UpdateAgentSessionID are safe unsynchronized only because
	// lockForTaskEdit is expected to serialize the calls that reach them —
	// which is exactly the property those tests exist to verify. Without
	// this mutex, a regressed lock would hand two goroutines concurrent
	// access to agentSessionIDs and the Go runtime would abort the test
	// binary with "concurrent map writes" instead of failing an assertion.
	mu sync.Mutex

	agentSessionIDs map[string]string
	updateErr       error

	lastCreateName, lastCreateSource          string
	lastEditSession, lastEditTask             string
	lastSaveSession, lastCancelSess           string
	lastUpdateSession, lastUpdateAgentSession string

	// A sandbox the write tool accepts is the default: every test that is
	// not about the boundary would otherwise have to opt in to a turn firing
	// at all.
	sandboxUnwritable bool
	sandboxRoots      []string
	lastSandboxDir    string
}

func (m *mockAuthoring) CreateTask(_ context.Context, name, source string) (AuthoringCreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCreateName, m.lastCreateSource = name, source
	return m.createResult, m.createErr
}

func (m *mockAuthoring) EditTask(_ context.Context, sessionID, taskID string) (AuthoringEditResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastEditSession, m.lastEditTask = sessionID, taskID
	res := m.editResult
	if asid, ok := m.agentSessionIDs[res.SessionID]; ok {
		res.AgentSessionID = asid
	}
	return res, m.editErr
}

func (m *mockAuthoring) SaveTask(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSaveSession = sessionID
	return m.saveErr
}

func (m *mockAuthoring) CancelTask(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCancelSess = sessionID
	return m.cancelErr
}

func (m *mockAuthoring) UpdateAgentSessionID(_ context.Context, sessionID, agentSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUpdateSession, m.lastUpdateAgentSession = sessionID, agentSessionID
	if m.updateErr != nil {
		return m.updateErr
	}
	if agentSessionID == "" {
		return nil
	}
	if m.agentSessionIDs == nil {
		m.agentSessionIDs = map[string]string{}
	}
	m.agentSessionIDs[sessionID] = agentSessionID
	return nil
}

func (m *mockAuthoring) SandboxWritable(dir string) (bool, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSandboxDir = dir
	return !m.sandboxUnwritable, m.sandboxRoots
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

// newAuthoringAIControl builds a ControlServer with a "buildin/task-create"
// task registered and defaultCreateTask pointed at it, so tests can exercise
// handleTaskEdit's/handleTaskCreate's AI-threading branch (#568) end to end
// with a fake engine — mirrors newAITestServer in control_ai_chat_test.go.
func newAuthoringAIControl(t *testing.T, a AuthoringService, eng EngineRunner) *ControlServer {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	if err := reg.Register(&task.Spec{
		ID: "buildin/task-create", Name: "task-create",
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return &ControlServer{
		authoring: a, engine: eng, reg: reg,
		defaultCreateTask: "buildin/task-create",
		log:               zap.NewNop(),
	}
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
	// The --ai path now actually threads the prompt through a real AI turn
	// (#568), so the chained edit needs a working defaultCreateTask + engine.
	eng := &promptCapturingEngine{reply: "scaffolded it", sessID: "asid-9"}
	cs := newAuthoringAIControl(t, m, eng)
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
	if res.Reply != "scaffolded it" {
		t.Errorf("Reply = %q, want the chained AI turn's reply", res.Reply)
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
	// Blank prompt: this test is about the session-open/URL-construction path,
	// not AI-threading (covered separately below), so it deliberately avoids
	// exercising cs.defaultCreateTask, which newAuthoringControl leaves unset.
	res, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t"})
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
	if res.Reply != "" || res.Suspended {
		t.Errorf("blank prompt must not fire an AI turn: res = %+v", res)
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

// ── AI-threading (#568) ──────────────────────────────────────────────────────

func TestControl_TaskEdit_NoPrompt_NoAITurn(t *testing.T) {
	// Even with defaultCreateTask configured, a blank prompt must not fire
	// anything — this is the pre-#568 plain-edit behavior every existing
	// caller of `dicode task edit <id>` (no prompt) still depends on.
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &oneShotEngine{}
	cs := newAuthoringAIControl(t, m, eng)

	res, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t"})
	if err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	if res.Reply != "" || res.RunID != "" {
		t.Errorf("blank prompt fired a turn: res = %+v", res)
	}
	if eng.firedParams != nil {
		t.Errorf("blank prompt called FireManual: params = %v", eng.firedParams)
	}
}

func TestControl_TaskEdit_NoDefaultCreateTask_Errors(t *testing.T) {
	// mirrors handleAI's guard for a blank defaultAITask: a non-empty prompt
	// with no create task configured is a configuration error, not a silent
	// no-op — the #568 fix is that the prompt is no longer just discarded.
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	cs := newAuthoringControl(m) // defaultCreateTask left unset
	_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "do it"})
	if err == nil || !strings.Contains(err.Error(), "no create task configured") {
		t.Fatalf("err = %v, want 'no create task configured'", err)
	}
}

func TestControl_TaskEdit_CreateTaskNotRegistered_Errors(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	cs := &ControlServer{
		authoring: m, engine: &oneShotEngine{}, reg: registry.New(d),
		defaultCreateTask: "buildin/task-create", // not registered on this reg
		log:               zap.NewNop(),
	}
	_, err = cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "do it"})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v, want 'not registered'", err)
	}
}

// promptCapturingEngine records the params passed to FireManual across
// multiple calls, so a test can assert both the per-turn params and the
// exact number of turns fired.
type promptCapturingEngine struct {
	mockEngine
	calls  []map[string]string
	status string // defaults to "success"
	reply  string
	sessID string
}

func (e *promptCapturingEngine) FireManual(_ context.Context, taskID string, params map[string]string) (string, error) {
	if taskID != "buildin/task-create" {
		return "", errors.New("unexpected task id: " + taskID)
	}
	e.calls = append(e.calls, params)
	return fmt.Sprintf("run-%d", len(e.calls)), nil
}

func (e *promptCapturingEngine) WaitRunSettled(_ context.Context, runID string) (RunResult, error) {
	status := e.status
	if status == "" {
		status = "success"
	}
	return RunResult{
		RunID:  runID,
		Status: status,
		ReturnValue: map[string]any{
			"reply":      e.reply,
			"session_id": e.sessID,
		},
	}, nil
}

func TestControl_TaskEdit_PromptFiresAITurn(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &promptCapturingEngine{reply: "here is your task", sessID: "asid-1"}
	cs := newAuthoringAIControl(t, m, eng)

	res, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "scaffold a slack notifier"})
	if err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	if res.Reply != "here is your task" {
		t.Errorf("Reply = %q, want %q", res.Reply, "here is your task")
	}
	if res.RunID == "" {
		t.Error("RunID should be populated on a successful turn")
	}
	if res.Suspended {
		t.Error("a successful run must not be marked Suspended")
	}
	if len(eng.calls) != 1 {
		t.Fatalf("FireManual calls = %d, want 1", len(eng.calls))
	}
	got := eng.calls[0]
	// #723: buildin/ai-agent declares no "task_id" param and never reads
	// one — a bare param would silently vanish (FireManual doesn't
	// validate against the task's schema) and the agent would never learn
	// its target. The target must ride inside "prompt" itself, where the
	// model actually sees it.
	wantPrompt := "(Target task: ai-scratch/t)\n\nscaffold a slack notifier"
	if got["prompt"] != wantPrompt {
		t.Errorf("prompt param = %q, want %q", got["prompt"], wantPrompt)
	}
	if _, ok := got["task_id"]; ok {
		t.Errorf("must not send a bare task_id param — ai-agent doesn't declare or read one (#723): params = %v", got)
	}
	// A pipeline stage's ${input.params.session_id} reference fails the
	// dispatch when the value is absent or empty, so the first turn mints one
	// rather than omitting it (#755).
	if got["session_id"] == "" {
		t.Errorf("first turn must send a freshly minted session_id: params = %v", got)
	}
	if got["task_dir"] == "" {
		t.Errorf("every turn must send a non-empty task_dir, sentinel included: params = %v", got)
	}
	// The turn's agent session id must have been persisted back onto the
	// authoring session for the next call to pick up.
	if m.lastUpdateSession != "s1" || m.lastUpdateAgentSession != "asid-1" {
		t.Errorf("UpdateAgentSessionID not called correctly: session=%q agentSession=%q", m.lastUpdateSession, m.lastUpdateAgentSession)
	}
}

func TestControl_TaskEdit_SessionIDCarriesAcrossCalls(t *testing.T) {
	// A second `dicode task edit <id> "<prompt>"` against the SAME open
	// authoring session must carry turn 1's agent_session_id along as the
	// session_id param on turn 2 (#568) — this proves run-group correlation
	// (the two turns' runs get grouped under one `chat:<id>` label for
	// UI/log display), NOT conversational memory: the underlying agent
	// still starts each turn from an empty SessionState (see
	// tasks/buildin/ai-agent/task.ts's oneShotTurn).
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &promptCapturingEngine{reply: "turn one done", sessID: "asid-1"}
	cs := newAuthoringAIControl(t, m, eng)

	if _, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "first message"}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}

	eng.reply = "turn two done"
	res, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "second message"})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res.Reply != "turn two done" {
		t.Errorf("turn 2 Reply = %q", res.Reply)
	}
	if len(eng.calls) != 2 {
		t.Fatalf("FireManual calls = %d, want 2", len(eng.calls))
	}
	if got := eng.calls[1]["session_id"]; got != "asid-1" {
		t.Errorf("turn 2 session_id param = %q, want asid-1 (carried from turn 1)", got)
	}
	if want, got := "(Target task: ai-scratch/t)\n\nsecond message", eng.calls[1]["prompt"]; got != want {
		t.Errorf("turn 2 prompt param = %q, want %q", got, want)
	}
}

func TestControl_TaskEdit_SuspendedRunSurfaces(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &promptCapturingEngine{status: registry.StatusSuspended}
	cs := newAuthoringAIControl(t, m, eng)

	res, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "need clarification"})
	if err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	if !res.Suspended {
		t.Error("suspended run must surface Suspended=true")
	}
	if res.RunID == "" {
		t.Error("RunID must be populated on a suspended run")
	}
}

func TestControl_TaskEdit_FailedRunSurfacesRunIDInError(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &promptCapturingEngine{status: "failure"}
	cs := newAuthoringAIControl(t, m, eng)

	_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "do it"})
	if err == nil {
		t.Fatal("expected error for a failed run")
	}
	if !strings.Contains(err.Error(), "run-1") || !strings.Contains(err.Error(), "failure") {
		t.Errorf("err = %v, want run id + status mentioned", err)
	}
}

func TestControl_TaskEdit_FireManualError_Surfaces(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	cs := newAuthoringAIControl(t, m, &erroringFireEngine{})
	_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "do it"})
	if err == nil || !strings.Contains(err.Error(), "fire boom") {
		t.Fatalf("err = %v, want fire error surfaced", err)
	}
}

type erroringFireEngine struct{ mockEngine }

func (e *erroringFireEngine) FireManual(context.Context, string, map[string]string) (string, error) {
	return "", errors.New("fire boom")
}

// gatedTurnEngine is a controllable EngineRunner for proving handleTaskEdit's
// per-session serialization (finding #4). FireManual reports each call's
// params on fireCh; WaitRunSettled blocks on proceed until the test lets it
// finish, so the test can pin exactly when a call is "in flight" (holding
// the session lock) versus attempting to start.
type gatedTurnEngine struct {
	mockEngine
	fireCh  chan map[string]string
	proceed chan struct{}
	sessID  string
	calls   int
}

func (e *gatedTurnEngine) FireManual(_ context.Context, taskID string, params map[string]string) (string, error) {
	if taskID != "buildin/task-create" {
		return "", errors.New("unexpected task id: " + taskID)
	}
	e.calls++
	e.fireCh <- params
	return fmt.Sprintf("run-%d", e.calls), nil
}

func (e *gatedTurnEngine) WaitRunSettled(_ context.Context, runID string) (RunResult, error) {
	<-e.proceed
	return RunResult{
		RunID:       runID,
		Status:      "success",
		ReturnValue: map[string]any{"reply": "ok", "session_id": e.sessID},
	}, nil
}

// TestControl_TaskEdit_ConcurrentSameSession_Serializes proves (not just
// "doesn't panic") that two concurrent handleTaskEdit calls against the SAME
// session/task serialize around the AgentSessionID read-fire-write sequence:
// the second call must observe the first call's persisted AgentSessionID
// rather than racing past it with a stale read (finding #4's TOCTOU).
func TestControl_TaskEdit_ConcurrentSameSession_Serializes(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &gatedTurnEngine{
		fireCh:  make(chan map[string]string),
		proceed: make(chan struct{}),
		sessID:  "asid-1",
	}
	cs := newAuthoringAIControl(t, m, eng)

	// Call 1 runs in its own goroutine; it blocks inside WaitRunSettled
	// until the test sends on `proceed`.
	call1Done := make(chan error, 1)
	go func() {
		_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "first message"})
		call1Done <- err
	}()

	// Wait for call 1 to actually fire — it now holds the session lock and
	// is parked in WaitRunSettled.
	select {
	case params1 := <-eng.fireCh:
		if params1["session_id"] == "" {
			t.Errorf("call 1 must mint a session_id for the first turn: %v", params1)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call 1 never fired")
	}

	// Start call 2 concurrently against the same task/session. If
	// serialization works it must block on the lock (still held by call 1)
	// rather than firing immediately.
	call2Done := make(chan error, 1)
	go func() {
		_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "second message"})
		call2Done <- err
	}()

	select {
	case params2 := <-eng.fireCh:
		t.Fatalf("call 2 fired while call 1 was still in flight — not serialized: %v", params2)
	case <-time.After(200 * time.Millisecond):
		// Expected: call 2 is blocked waiting on the session lock.
	}

	// Release call 1: it finishes its turn, persists AgentSessionID via
	// UpdateAgentSessionID, and releases the lock.
	eng.proceed <- struct{}{}
	if err := <-call1Done; err != nil {
		t.Fatalf("call 1: %v", err)
	}

	// Call 2 can now acquire the lock, read the session via EditTask, and
	// fire — its params must carry call 1's persisted AgentSessionID.
	var params2 map[string]string
	select {
	case params2 = <-eng.fireCh:
	case <-time.After(2 * time.Second):
		t.Fatal("call 2 never fired after call 1 released the lock")
	}
	eng.proceed <- struct{}{}
	if err := <-call2Done; err != nil {
		t.Fatalf("call 2: %v", err)
	}

	if got := params2["session_id"]; got != "asid-1" {
		t.Errorf("call 2 session_id param = %q, want asid-1 (call 1's persisted AgentSessionID, not a stale read)", got)
	}
}

// TestControl_TaskEdit_ConcurrentTaskOnlyAndSession_Serializes covers the gap
// TestControl_TaskEdit_ConcurrentSameSession_Serializes doesn't: two DIFFERENT
// request shapes that EditTask resolves to the SAME open session must still
// serialize against each other. `dicode task edit <id> "<prompt>"` sends
// TaskID only; `dicode task edit <id> "<prompt>" --session <sid>` sends both.
// lockForTaskEdit must canonicalize on source (derived from TaskID) rather
// than keying on whichever of SessionID/TaskID happened to be set, or these
// two calls would land on different locks and race right past each other on
// the one session they both actually touch.
func TestControl_TaskEdit_ConcurrentTaskOnlyAndSession_Serializes(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &gatedTurnEngine{
		fireCh:  make(chan map[string]string),
		proceed: make(chan struct{}),
		sessID:  "asid-1",
	}
	cs := newAuthoringAIControl(t, m, eng)

	// Call 1: task-only shape (no --session).
	call1Done := make(chan error, 1)
	go func() {
		_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "first message"})
		call1Done <- err
	}()
	select {
	case <-eng.fireCh:
	case <-time.After(2 * time.Second):
		t.Fatal("call 1 never fired")
	}

	// Call 2: task+session shape, resolving (per mockAuthoring.EditTask,
	// which ignores its inputs and always returns editResult) to the exact
	// same session as call 1. Must block, not race ahead.
	call2Done := make(chan error, 1)
	go func() {
		_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", SessionID: "s1", Prompt: "second message"})
		call2Done <- err
	}()

	select {
	case params2 := <-eng.fireCh:
		t.Fatalf("call 2 (task+session shape) fired while call 1 (task-only shape) was still in flight — different lock keys for the same session: %v", params2)
	case <-time.After(200 * time.Millisecond):
		// Expected: blocked on the same lock as call 1.
	}

	eng.proceed <- struct{}{}
	if err := <-call1Done; err != nil {
		t.Fatalf("call 1: %v", err)
	}

	select {
	case <-eng.fireCh:
	case <-time.After(2 * time.Second):
		t.Fatal("call 2 never fired after call 1 released the lock")
	}
	eng.proceed <- struct{}{}
	if err := <-call2Done; err != nil {
		t.Fatalf("call 2: %v", err)
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

// A write tool is useless without a target. The agent's only clue about where
// the task's files live is the prompt — list_tasks withholds TaskDir and the
// model has no way to ask for it (#734).
func TestControl_TaskEdit_PromptCarriesTaskDir(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{
		SessionID: "s1", TaskID: "ai-scratch/t", TaskDir: "/data/ai-tasks/t",
	}}
	eng := &promptCapturingEngine{reply: "ok", sessID: "asid-1"}
	cs := newAuthoringAIControl(t, m, eng)

	if _, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "scaffold it"}); err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	got := eng.calls[0]["prompt"]
	if !strings.Contains(got, "/data/ai-tasks/t") {
		t.Errorf("prompt param = %q, want it to name the task directory", got)
	}
	if !strings.Contains(got, "ai-scratch/t") {
		t.Errorf("prompt param = %q, want it to keep naming the target task", got)
	}
	if !strings.HasSuffix(got, "\n\nscaffold it") {
		t.Errorf("prompt param = %q, want the user's prompt kept verbatim after the header", got)
	}
}

// A source whose directory can't be resolved must still fire a usable turn,
// naming the target task on its own.
func TestControl_TaskEdit_PromptOmitsEmptyTaskDir(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &promptCapturingEngine{reply: "ok", sessID: "asid-1"}
	cs := newAuthoringAIControl(t, m, eng)

	if _, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "scaffold it"}); err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	if want, got := "(Target task: ai-scratch/t)\n\nscaffold it", eng.calls[0]["prompt"]; got != want {
		t.Errorf("prompt param = %q, want %q", got, want)
	}
}

// TestControl_TaskEdit_UnresolvedTaskDirTravelsAsSentinel: buildin/task-create
// is a pipeline, and a stage's ${input.params.task_dir} reference fails the
// dispatch on an empty value. An unresolved directory therefore has to travel
// as a sentinel the post-condition stage can recognise, not as an omission
// that would fail the turn outright (#755).
func TestControl_TaskEdit_UnresolvedTaskDirTravelsAsSentinel(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t"}}
	eng := &promptCapturingEngine{reply: "done"}
	cs := newAuthoringAIControl(t, m, eng)

	if _, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "write it"}); err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	if got := eng.calls[0]["task_dir"]; got != unresolvedTaskDir {
		t.Errorf("task_dir = %q, want the %q sentinel", got, unresolvedTaskDir)
	}
}

// TestControl_TaskEdit_ResolvedTaskDirTravelsVerbatim guards the other half:
// the sentinel must never stand in for a directory that did resolve.
func TestControl_TaskEdit_ResolvedTaskDirTravelsVerbatim(t *testing.T) {
	m := &mockAuthoring{editResult: AuthoringEditResult{SessionID: "s1", TaskID: "ai-scratch/t", TaskDir: "/srv/tasks/t"}}
	eng := &promptCapturingEngine{reply: "done"}
	cs := newAuthoringAIControl(t, m, eng)

	if _, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "ai-scratch/t", Prompt: "write it"}); err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	if got := eng.calls[0]["task_dir"]; got != "/srv/tasks/t" {
		t.Errorf("task_dir = %q, want the resolved directory", got)
	}
}

// The agent's write tool refuses a path outside its declared roots, and it
// refuses it as a tool result the model can narrate its way past — so a
// session with no writable sandbox must not reach the model at all.
func TestControl_TaskEdit_RefusesUnwritableSandbox(t *testing.T) {
	m := &mockAuthoring{
		editResult: AuthoringEditResult{
			SessionID:   "sess-1",
			TaskID:      "scratch/zen-quote",
			TaskDir:     "/tmp/dc-ai/src/zen-quote",
			SandboxPath: "/tmp/dc-ai/src/zen-quote",
		},
		sandboxUnwritable: true,
		sandboxRoots:      []string{"/tmp/dc-ai/ai-tasks"},
	}
	eng := &promptCapturingEngine{reply: "wrote it"}
	cs := newAuthoringAIControl(t, m, eng)

	_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "scratch/zen-quote", Prompt: "fetch the zen api"})
	if err == nil {
		t.Fatal("handleTaskEdit fired a turn into an unwritable sandbox, want a refusal")
	}
	// The operator has to be able to act on this: it names the directory that
	// was refused, the roots it was measured against, and the entry to override.
	for _, want := range []string{"/tmp/dc-ai/src/zen-quote", "/tmp/dc-ai/ai-tasks", "buildin/write-task-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if len(eng.calls) != 0 {
		t.Errorf("fired %d turns despite the refusal", len(eng.calls))
	}
	if m.lastSandboxDir != "/tmp/dc-ai/src/zen-quote" {
		t.Errorf("checked sandbox %q, want the session's own", m.lastSandboxDir)
	}
}

// A promptless edit only opens the session — there is no turn to protect, and
// refusing it would break `dicode task edit <id>` against every source whose
// root the operator has not widened.
func TestControl_TaskEdit_UnwritableSandboxStillOpensSession(t *testing.T) {
	m := &mockAuthoring{
		editResult:        AuthoringEditResult{SessionID: "sess-1", TaskID: "scratch/zen-quote", SandboxPath: "/tmp/dc-ai/src/zen-quote"},
		sandboxUnwritable: true,
	}
	cs := newAuthoringAIControl(t, m, &promptCapturingEngine{})

	res, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "scratch/zen-quote"})
	if err != nil {
		t.Fatalf("handleTaskEdit: %v", err)
	}
	if res.SessionID != "sess-1" {
		t.Errorf("session id = %q, want sess-1", res.SessionID)
	}
}

// With no write tool registered there is no root to name, so the refusal has
// to say that rather than point at an empty list.
func TestControl_TaskEdit_RefusalNamesAMissingWriteTool(t *testing.T) {
	m := &mockAuthoring{
		editResult:        AuthoringEditResult{SessionID: "sess-1", TaskID: "scratch/zen", SandboxPath: "/tmp/dc-ai/src/zen"},
		sandboxUnwritable: true,
	}
	cs := newAuthoringAIControl(t, m, &promptCapturingEngine{})

	_, err := cs.handleTaskEdit(context.Background(), Request{TaskID: "scratch/zen", Prompt: "go"})
	if err == nil {
		t.Fatal("handleTaskEdit fired a turn with no write tool, want a refusal")
	}
	if !strings.Contains(err.Error(), "declares no writable root") || strings.Contains(err.Error(), "[]") {
		t.Errorf("error = %q, want the missing-write-tool wording and no empty root list", err)
	}
}
