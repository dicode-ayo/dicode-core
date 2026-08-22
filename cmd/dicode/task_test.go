package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// fakeAuthoring is an in-process ipc.AuthoringService used to drive the CLI
// verbs end-to-end over a real control socket, so the tests exercise the full
// flag-parse → IPC → handler → stdout/stderr-split chain.
type fakeAuthoring struct {
	create    ipc.AuthoringCreateResult
	createErr error
	edit      ipc.AuthoringEditResult
	editErr   error
	saveErr   error
	cancelErr error
}

func (f *fakeAuthoring) CreateTask(_ context.Context, name, source string) (ipc.AuthoringCreateResult, error) {
	if f.createErr != nil {
		return ipc.AuthoringCreateResult{}, f.createErr
	}
	res := f.create
	if res.TaskID == "" {
		res.TaskID = source + "/" + name
	}
	return res, nil
}

func (f *fakeAuthoring) EditTask(_ context.Context, sessionID, taskID string) (ipc.AuthoringEditResult, error) {
	if f.editErr != nil {
		return ipc.AuthoringEditResult{}, f.editErr
	}
	return f.edit, nil
}

func (f *fakeAuthoring) SaveTask(_ context.Context, sessionID string) error   { return f.saveErr }
func (f *fakeAuthoring) CancelTask(_ context.Context, sessionID string) error { return f.cancelErr }
func (f *fakeAuthoring) UpdateAgentSessionID(_ context.Context, sessionID, agentSessionID string) error {
	return nil
}
func (f *fakeAuthoring) WebUIBaseURL() string { return "http://localhost:8080" }

// dialTestClient boots a ControlServer wired to auth and returns a connected
// ControlClient plus a cleanup func. A "buildin/task-create" task is
// registered and the server's create-task default points at it, backed by
// aiTurnEngine, so any prompt threaded through cmdTaskCreate --ai /
// cmdTaskEdit's positional prompt args (#568) fires a real (fake) turn
// instead of tripping the "no create task configured" guard — most of these
// tests exist to check flag-parsing and stdout/stderr splitting, not the
// AI-threading wiring itself, which pkg/ipc's control_task_authoring_test.go
// covers directly.
func dialTestClient(t *testing.T, auth ipc.AuthoringService) (*ipc.ControlClient, func()) {
	t.Helper()
	return dialTestClientWithEngine(t, auth, &aiTurnEngine{})
}

// dialTestClientWithEngine is dialTestClient with the fake engine
// parameterized, so a test that needs a specific AI-turn outcome (a real
// non-empty reply, a particular session id, ...) can supply its own
// ipc.EngineRunner instead of always getting aiTurnEngine's fixed empty
// ReturnValue (#568 finding 6 — see replyingEngine below).
func dialTestClientWithEngine(t *testing.T, auth ipc.AuthoringService, eng ipc.EngineRunner) (*ipc.ControlClient, func()) {
	t.Helper()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

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
		t.Fatalf("register task-create: %v", err)
	}

	cs, err := ipc.NewControlServer(socketPath, tokenPath, reg, eng, nil, ipc.MetricsProvider{}, "test", zap.NewNop(), nil, "", "buildin/task-create")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}
	cs.SetAuthoringService(auth)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = cs.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("control socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}

	c, err := ipc.Dial(socketPath, tokenPath)
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	return c, func() { c.Close(); cancel() }
}

// noopEngine satisfies ipc.EngineRunner for tests that don't fire tasks.
type noopEngine struct{}

func (noopEngine) FireManual(context.Context, string, map[string]string) (string, error) {
	return "", nil
}
func (noopEngine) FireFromTask(context.Context, string, string, map[string]string) (string, error) {
	return "", nil
}
func (noopEngine) WaitRun(context.Context, string) (ipc.RunResult, error) {
	return ipc.RunResult{}, nil
}
func (noopEngine) WaitRunSettled(context.Context, string) (ipc.RunResult, error) {
	return ipc.RunResult{}, nil
}
func (noopEngine) KillRun(string) bool     { return false }
func (noopEngine) ActiveRunCount() int     { return 0 }
func (noopEngine) ActiveTaskSlots() int    { return 0 }
func (noopEngine) MaxConcurrentTasks() int { return 0 }
func (noopEngine) WaitingTasks() int       { return 0 }

// aiTurnEngine embeds noopEngine but fires and settles a successful AI turn,
// so dialTestClient's registered "buildin/task-create" can actually be
// "fired" by handleTaskEdit's prompt-threading branch (#568) without any
// individual test having to wire its own engine just to get past the guard.
type aiTurnEngine struct {
	noopEngine
}

func (aiTurnEngine) FireManual(context.Context, string, map[string]string) (string, error) {
	return "run-cli-ai", nil
}
func (aiTurnEngine) WaitRunSettled(context.Context, string) (ipc.RunResult, error) {
	return ipc.RunResult{RunID: "run-cli-ai", Status: "success", ReturnValue: map[string]any{}}, nil
}

// replyingEngine is aiTurnEngine's configurable sibling: it fires and
// settles a successful AI turn that returns a caller-supplied {reply,
// session_id}, so tests can verify a REAL non-empty reply travels the full
// control-socket round trip into the right CLI output stream (#568 finding
// 6) — aiTurnEngine's always-empty ReturnValue can't exercise that.
type replyingEngine struct {
	noopEngine
	reply  string
	sessID string
}

func (e *replyingEngine) FireManual(context.Context, string, map[string]string) (string, error) {
	return "run-cli-reply", nil
}

func (e *replyingEngine) WaitRunSettled(context.Context, string) (ipc.RunResult, error) {
	return ipc.RunResult{
		RunID:       "run-cli-reply",
		Status:      "success",
		ReturnValue: map[string]any{"reply": e.reply, "session_id": e.sessID},
	}, nil
}

// suspendingEngine fires an AI turn whose run suspends awaiting further
// input, so tests can verify cmdTaskEdit/cmdTaskCreate surface a suspended
// run instead of silently printing an empty reply as if the turn finished
// (#568 finding 3).
type suspendingEngine struct {
	noopEngine
}

func (suspendingEngine) FireManual(context.Context, string, map[string]string) (string, error) {
	return "run-cli-suspended", nil
}

func (suspendingEngine) WaitRunSettled(context.Context, string) (ipc.RunResult, error) {
	return ipc.RunResult{RunID: "run-cli-suspended", Status: registry.StatusSuspended}, nil
}

// captureOutput redirects os.Stdout and os.Stderr around fn and returns what
// each captured. The CLI verbs write directly to the process streams, so this
// is how the stdout/stderr split is asserted.
func captureOutput(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr

	err = fn()

	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr

	var bo, be bytes.Buffer
	_, _ = io.Copy(&bo, rOut)
	_, _ = io.Copy(&be, rErr)
	return bo.String(), be.String(), err
}

// --- flag-parsing tests (no socket needed: they fail before Send) ----------

func TestCmdTaskCreate_MissingName(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{})
	defer done()
	_, _, err := captureOutput(t, func() error { return cmdTaskCreate(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("err = %v, want usage", err)
	}
}

func TestCmdTaskCreate_DanglingFlag(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{})
	defer done()
	_, _, err := captureOutput(t, func() error { return cmdTaskCreate(c, []string{"name", "--source"}) })
	if err == nil || !strings.Contains(err.Error(), "--source requires a value") {
		t.Fatalf("err = %v", err)
	}
}

func TestCmdTaskCreate_Plain(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{
		create: ipc.AuthoringCreateResult{TaskID: "ai-scratch/hello", Source: "ai-scratch"},
	})
	defer done()
	out, errOut, err := captureOutput(t, func() error {
		return cmdTaskCreate(c, []string{"hello", "--source", "ai-scratch"})
	})
	if err != nil {
		t.Fatalf("cmdTaskCreate: %v", err)
	}
	if strings.TrimSpace(out) != "ai-scratch/hello" {
		t.Errorf("stdout = %q, want task id", out)
	}
	if errOut != "" {
		t.Errorf("plain create should have empty stderr, got %q", errOut)
	}
}

func TestCmdTaskCreate_WithAI_SplitsStreams(t *testing.T) {
	// The --ai metadata (session, url, reply) is produced by the daemon
	// chaining create → edit; the edit result drives it.
	c, done := dialTestClient(t, &fakeAuthoring{
		create: ipc.AuthoringCreateResult{TaskID: "ai-scratch/hello", Source: "ai-scratch"},
		edit:   ipc.AuthoringEditResult{SessionID: "sess-1", Source: "ai-scratch", SourceKind: "local"},
	})
	defer done()
	out, errOut, err := captureOutput(t, func() error {
		return cmdTaskCreate(c, []string{"hello", "--ai", "make it greet"})
	})
	if err != nil {
		t.Fatalf("cmdTaskCreate: %v", err)
	}
	if strings.TrimSpace(out) != "ai-scratch/hello" {
		t.Errorf("stdout = %q, want only task id", out)
	}
	if !strings.Contains(errOut, "session: sess-1") {
		t.Errorf("stderr missing session line: %q", errOut)
	}
	if !strings.Contains(errOut, "open: http://localhost:8080/?session=sess-1") {
		t.Errorf("stderr missing open line: %q", errOut)
	}
}

// TestCmdTaskCreate_WithAI_ReplyReachesStderr drives the full control-socket
// round trip (create chains into edit inside the daemon) with an engine that
// returns a real non-empty reply, and asserts that exact text lands on
// stderr while stdout stays the bare task id — matching cmdTaskCreate's
// documented stdout/stderr convention (#568 finding 6). dialTestClient's
// default aiTurnEngine always returns an empty ReturnValue, so no other test
// in this file exercises a real reply reaching the chained --ai path.
func TestCmdTaskCreate_WithAI_ReplyReachesStderr(t *testing.T) {
	eng := &replyingEngine{reply: "scaffolded your greeting task", sessID: "asid-cli-2"}
	c, done := dialTestClientWithEngine(t, &fakeAuthoring{
		create: ipc.AuthoringCreateResult{TaskID: "ai-scratch/hello", Source: "ai-scratch"},
		edit:   ipc.AuthoringEditResult{SessionID: "sess-1", Source: "ai-scratch", SourceKind: "local"},
	}, eng)
	defer done()
	out, errOut, err := captureOutput(t, func() error {
		return cmdTaskCreate(c, []string{"hello", "--ai", "make it greet"})
	})
	if err != nil {
		t.Fatalf("cmdTaskCreate: %v", err)
	}
	if strings.TrimSpace(out) != "ai-scratch/hello" {
		t.Errorf("stdout = %q, want only the task id", out)
	}
	if !strings.Contains(errOut, "scaffolded your greeting task") {
		t.Errorf("stderr missing AI reply: %q", errOut)
	}
}

// TestCmdTaskCreate_WithAI_SuspendedRunSurfacesOnStderr asserts cmdTaskCreate
// checks res.Suspended (#568 finding 3): a suspended chained turn must print
// a clear stderr message with the run id and a resume hint instead of
// silently falling through as if the turn succeeded — but the scaffolded
// task id still reaches stdout, since the file did land on disk.
func TestCmdTaskCreate_WithAI_SuspendedRunSurfacesOnStderr(t *testing.T) {
	c, done := dialTestClientWithEngine(t, &fakeAuthoring{
		create: ipc.AuthoringCreateResult{TaskID: "ai-scratch/hello", Source: "ai-scratch"},
		edit:   ipc.AuthoringEditResult{SessionID: "sess-1", Source: "ai-scratch", SourceKind: "local"},
	}, &suspendingEngine{})
	defer done()
	out, errOut, err := captureOutput(t, func() error {
		return cmdTaskCreate(c, []string{"hello", "--ai", "make it greet"})
	})
	if err != nil {
		t.Fatalf("cmdTaskCreate: %v", err)
	}
	if strings.TrimSpace(out) != "ai-scratch/hello" {
		t.Errorf("stdout = %q, want the scaffolded task id even though the turn suspended", out)
	}
	if !strings.Contains(errOut, "run-cli-suspended") {
		t.Errorf("stderr missing suspended run id: %q", errOut)
	}
	if !strings.Contains(errOut, "dicode resume") {
		t.Errorf("stderr missing resume hint: %q", errOut)
	}
}

func TestCmdTaskCreate_AIPathChainsEditWithEqualsFlags(t *testing.T) {
	fa := &recordingAuthoring{fakeAuthoring: fakeAuthoring{
		create: ipc.AuthoringCreateResult{TaskID: "ai-scratch/x"},
		edit:   ipc.AuthoringEditResult{SessionID: "s"},
	}}
	c, done := dialTestClient(t, fa)
	defer done()
	_, _, err := captureOutput(t, func() error {
		return cmdTaskCreate(c, []string{"x", "--ai=do it", "--source=ai-scratch"})
	})
	if err != nil {
		t.Fatalf("cmdTaskCreate: %v", err)
	}
	if fa.source != "ai-scratch" {
		t.Errorf("source forwarded = %q, want ai-scratch", fa.source)
	}
	// --ai presence makes the daemon chain create → edit against the new task.
	if fa.editTask != "ai-scratch/x" {
		t.Errorf("edit chained against %q, want ai-scratch/x", fa.editTask)
	}
}

func TestCmdTaskCreate_AIEditFailureSurfacesTaskID(t *testing.T) {
	// Drives the real control socket: create succeeds, the chained edit fails.
	// The dispatch loop sends Error XOR Result, so the created task id must ride
	// inside the error string to reach the CLI user (a retry command).
	c, done := dialTestClient(t, &fakeAuthoring{
		create:  ipc.AuthoringCreateResult{TaskID: "ai-scratch/hello", Source: "ai-scratch"},
		editErr: errors.New("agent unavailable"),
	})
	defer done()
	out, _, err := captureOutput(t, func() error {
		return cmdTaskCreate(c, []string{"hello", "--ai", "make it greet"})
	})
	if err == nil {
		t.Fatal("expected an error when the chained edit fails")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ai-scratch/hello") {
		t.Errorf("error must contain the created task id, got %q", msg)
	}
	if !strings.Contains(msg, "dicode task edit ai-scratch/hello") {
		t.Errorf("error must contain a ready-to-run retry command, got %q", msg)
	}
	// stdout stays empty on failure — the task id reaches the user via stderr/error.
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout should be empty on failure, got %q", out)
	}
}

func TestCmdTaskEdit_MissingTaskID(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{})
	defer done()
	_, _, err := captureOutput(t, func() error { return cmdTaskEdit(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("err = %v", err)
	}
}

func TestCmdTaskEdit_SplitsStreams(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{
		edit: ipc.AuthoringEditResult{SessionID: "e1", Source: "ai-scratch", SourceKind: "local"},
	})
	defer done()
	out, errOut, err := captureOutput(t, func() error {
		return cmdTaskEdit(c, []string{"ai-scratch/t", "please", "fix", "it"})
	})
	if err != nil {
		t.Fatalf("cmdTaskEdit: %v", err)
	}
	// Reply (empty here) goes to stdout; session + open go to stderr.
	if !strings.Contains(errOut, "session: e1") {
		t.Errorf("stderr missing session: %q", errOut)
	}
	if !strings.Contains(errOut, "open: http://localhost:8080/?session=e1") {
		t.Errorf("stderr missing open url: %q", errOut)
	}
	_ = out
}

// TestCmdTaskEdit_AIReplyReachesStdout drives the full control-socket round
// trip with an engine that returns a real non-empty reply, and asserts that
// exact text lands on stdout (the piped-value convention documented at the
// top of this file) — dialTestClient's default aiTurnEngine always returns
// an empty ReturnValue, so no other test in this file exercises this (#568
// finding 6).
func TestCmdTaskEdit_AIReplyReachesStdout(t *testing.T) {
	eng := &replyingEngine{reply: "here is your edited task", sessID: "asid-cli-1"}
	c, done := dialTestClientWithEngine(t, &fakeAuthoring{
		edit: ipc.AuthoringEditResult{SessionID: "e1", Source: "ai-scratch", SourceKind: "local"},
	}, eng)
	defer done()
	out, errOut, err := captureOutput(t, func() error {
		return cmdTaskEdit(c, []string{"ai-scratch/t", "please", "fix", "it"})
	})
	if err != nil {
		t.Fatalf("cmdTaskEdit: %v", err)
	}
	if strings.TrimSpace(out) != "here is your edited task" {
		t.Errorf("stdout = %q, want the AI reply", out)
	}
	if strings.Contains(errOut, "here is your edited task") {
		t.Errorf("reply leaked into stderr: %q", errOut)
	}
}

// TestCmdTaskEdit_SuspendedRunSurfacesOnStderr asserts cmdTaskEdit checks
// res.Suspended (#568 finding 3): a suspended run must print a clear message
// with the run id and a resume hint on stderr, and must not print anything
// on stdout as if the turn had completed normally.
func TestCmdTaskEdit_SuspendedRunSurfacesOnStderr(t *testing.T) {
	c, done := dialTestClientWithEngine(t, &fakeAuthoring{
		edit: ipc.AuthoringEditResult{SessionID: "e1", Source: "ai-scratch", SourceKind: "local"},
	}, &suspendingEngine{})
	defer done()
	out, errOut, err := captureOutput(t, func() error {
		return cmdTaskEdit(c, []string{"ai-scratch/t", "clarify", "something"})
	})
	if err != nil {
		t.Fatalf("cmdTaskEdit: %v", err)
	}
	if !strings.Contains(errOut, "run-cli-suspended") {
		t.Errorf("stderr missing suspended run id: %q", errOut)
	}
	if !strings.Contains(errOut, "dicode resume") {
		t.Errorf("stderr missing resume hint: %q", errOut)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout should stay empty on a suspended run, got %q", out)
	}
}

func TestCmdTaskEdit_DashSentinel(t *testing.T) {
	fa := &recordingAuthoring{fakeAuthoring: fakeAuthoring{
		edit: ipc.AuthoringEditResult{SessionID: "e1"},
	}}
	c, done := dialTestClient(t, fa)
	defer done()
	// After `--`, "--session" is a literal prompt word, not a flag.
	_, _, err := captureOutput(t, func() error {
		return cmdTaskEdit(c, []string{"ai-scratch/t", "--", "--session", "is", "a", "word"})
	})
	if err != nil {
		t.Fatalf("cmdTaskEdit: %v", err)
	}
	if fa.editSession != "" {
		t.Errorf("--session after -- must not set sessionID, got %q", fa.editSession)
	}
}

func TestCmdTaskEdit_SessionFlag(t *testing.T) {
	fa := &recordingAuthoring{fakeAuthoring: fakeAuthoring{
		edit: ipc.AuthoringEditResult{SessionID: "resumed"},
	}}
	c, done := dialTestClient(t, fa)
	defer done()
	_, _, err := captureOutput(t, func() error {
		return cmdTaskEdit(c, []string{"ai-scratch/t", "more", "--session", "resumed"})
	})
	if err != nil {
		t.Fatalf("cmdTaskEdit: %v", err)
	}
	if fa.editSession != "resumed" {
		t.Errorf("sessionID forwarded = %q, want resumed", fa.editSession)
	}
}

func TestCmdTaskSave_MissingArg(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{})
	defer done()
	_, _, err := captureOutput(t, func() error { return cmdTaskSave(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("err = %v", err)
	}
}

func TestCmdTaskSave_PrintsSessionToStderr(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{})
	defer done()
	out, errOut, err := captureOutput(t, func() error { return cmdTaskSave(c, []string{"s1"}) })
	if err != nil {
		t.Fatalf("cmdTaskSave: %v", err)
	}
	if !strings.Contains(errOut, "session: s1") {
		t.Errorf("stderr = %q", errOut)
	}
	// No PR url / task id from the fake → stdout falls back to the session id.
	if strings.TrimSpace(out) != "s1" {
		t.Errorf("stdout = %q", out)
	}
}

func TestCmdTaskCancel_PrintsConfirmation(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{})
	defer done()
	out, errOut, err := captureOutput(t, func() error { return cmdTaskCancel(c, []string{"s2"}) })
	if err != nil {
		t.Fatalf("cmdTaskCancel: %v", err)
	}
	if !strings.Contains(out, "cancelled s2") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errOut, "session: s2") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestCmdTaskCancel_MissingArg(t *testing.T) {
	c, done := dialTestClient(t, &fakeAuthoring{})
	defer done()
	_, _, err := captureOutput(t, func() error { return cmdTaskCancel(c, nil) })
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("err = %v", err)
	}
}

// recordingAuthoring captures the arguments forwarded to the service so the
// CLI tests can assert flag wiring (prompt, source, sessionID).
type recordingAuthoring struct {
	fakeAuthoring
	source, editSession, editTask string
}

func (r *recordingAuthoring) CreateTask(ctx context.Context, name, source string) (ipc.AuthoringCreateResult, error) {
	r.source = source
	return r.fakeAuthoring.CreateTask(ctx, name, source)
}

func (r *recordingAuthoring) EditTask(ctx context.Context, sessionID, taskID string) (ipc.AuthoringEditResult, error) {
	// CreateTask's --ai chain forwards the prompt via EditTask on the daemon
	// side, so record both the prompt-carrying edit and direct edits here.
	r.editSession, r.editTask = sessionID, taskID
	return r.fakeAuthoring.EditTask(ctx, sessionID, taskID)
}

// scaffoldTaskDir writes the two files CreateTask scaffolds, giving the #755
// post-condition a real directory to snapshot on both sides of a turn.
func scaffoldTaskDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("apiVersion: dicode/v1\nkind: Task\nname: t\n"), 0644); err != nil {
		t.Fatalf("write task.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.js"), []byte("export default async function main() {}\n"), 0644); err != nil {
		t.Fatalf("write task.js: %v", err)
	}
	return dir
}

// writingEngine settles a successful AI turn that also writes a file into
// dir, standing in for an agent that actually called the write tool.
type writingEngine struct {
	noopEngine
	dir string
}

func (e *writingEngine) FireManual(context.Context, string, map[string]string) (string, error) {
	return "run-cli-wrote", os.WriteFile(filepath.Join(e.dir, "task.ts"), []byte("export default async function main() {}\n"), 0644)
}

func (e *writingEngine) WaitRunSettled(context.Context, string) (ipc.RunResult, error) {
	return ipc.RunResult{
		RunID:       "run-cli-wrote",
		Status:      "success",
		ReturnValue: map[string]any{"reply": "wrote the task"},
	}, nil
}

// TestCmdTaskEdit_TurnThatWroteNothing_Fails: the agent reports work it never
// did and the run succeeds; the command must still not exit 0 (#755).
func TestCmdTaskEdit_TurnThatWroteNothing_Fails(t *testing.T) {
	dir := scaffoldTaskDir(t)
	c, done := dialTestClientWithEngine(t, &fakeAuthoring{
		edit: ipc.AuthoringEditResult{SessionID: "sess-1", TaskID: "ai-scratch/zen", TaskDir: dir},
	}, &replyingEngine{reply: "I created task.yaml, task.ts and task.test.ts. The tests pass."})
	defer done()

	out, _, err := captureOutput(t, func() error {
		return cmdTaskEdit(c, []string{"ai-scratch/zen", "fetch the zen endpoint daily"})
	})
	if err == nil {
		t.Fatal("a turn that wrote no files must not exit 0")
	}
	if !strings.Contains(err.Error(), "ai-scratch/zen") || !strings.Contains(err.Error(), "no files") {
		t.Errorf("error must name the task and say no files were written, got %q", err)
	}
	if !strings.Contains(out, "task.test.ts") {
		t.Errorf("the agent's reply must still be printed as evidence, got stdout %q", out)
	}
}

// TestCmdTaskEdit_TurnThatWrote_ListsFilesAndSucceeds: files moved, so the
// command succeeds and names them.
func TestCmdTaskEdit_TurnThatWrote_ListsFilesAndSucceeds(t *testing.T) {
	dir := scaffoldTaskDir(t)
	c, done := dialTestClientWithEngine(t, &fakeAuthoring{
		edit: ipc.AuthoringEditResult{SessionID: "sess-1", TaskID: "ai-scratch/zen", TaskDir: dir},
	}, &writingEngine{dir: dir})
	defer done()

	_, errOut, err := captureOutput(t, func() error {
		return cmdTaskEdit(c, []string{"ai-scratch/zen", "fetch the zen endpoint daily"})
	})
	if err != nil {
		t.Fatalf("cmdTaskEdit: %v", err)
	}
	if !strings.Contains(errOut, "wrote: task.ts") {
		t.Errorf("stderr must name the files the turn wrote, got %q", errOut)
	}
}

// TestCmdTaskCreate_WithAI_TurnThatWroteNothing_FailsButKeepsTaskID: the
// scaffold landed and the task is registered, so the id still belongs on
// stdout; only the exit status changes (#755).
func TestCmdTaskCreate_WithAI_TurnThatWroteNothing_FailsButKeepsTaskID(t *testing.T) {
	dir := scaffoldTaskDir(t)
	c, done := dialTestClientWithEngine(t, &fakeAuthoring{
		create: ipc.AuthoringCreateResult{TaskID: "ai-scratch/zen", Source: "ai-scratch"},
		edit:   ipc.AuthoringEditResult{SessionID: "sess-1", TaskID: "ai-scratch/zen", TaskDir: dir},
	}, &replyingEngine{reply: "All three files are written."})
	defer done()

	out, _, err := captureOutput(t, func() error {
		return cmdTaskCreate(c, []string{"zen", "--ai", "fetch the zen endpoint daily"})
	})
	if err == nil {
		t.Fatal("create --ai whose turn wrote no files must not exit 0")
	}
	if strings.TrimSpace(out) != "ai-scratch/zen" {
		t.Errorf("stdout = %q, want the scaffolded task id — it does exist", out)
	}
}
