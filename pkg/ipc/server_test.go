package ipc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// ── test helpers ─────────────────────────────────────────────────────────────

type mockEngine struct {
	runID  string
	result RunResult
	err    error
}

func (m *mockEngine) FireManual(_ context.Context, _ string, _ map[string]string) (string, error) {
	return m.runID, m.err
}
func (m *mockEngine) WaitRun(_ context.Context, _ string) (RunResult, error) {
	return m.result, m.err
}
func (m *mockEngine) ActiveRunCount() int     { return 0 }
func (m *mockEngine) ActiveTaskSlots() int    { return 0 }
func (m *mockEngine) MaxConcurrentTasks() int { return 0 }
func (m *mockEngine) WaitingTasks() int       { return 0 }

// sendMsg writes a length-prefixed JSON message to conn.
func sendMsg(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	if err := writeMsg(conn, v); err != nil {
		t.Fatalf("sendMsg: %v", err)
	}
}

// recvMsg reads a length-prefixed JSON message from conn into a raw map.
func recvMsg(t *testing.T, conn net.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var m map[string]any
	if err := readMsg(conn, &m); err != nil {
		t.Fatalf("recvMsg: %v", err)
	}
	return m
}

// dial connects to the Unix socket, retrying for up to 2 seconds.
func dial(t *testing.T, socketPath string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			return conn
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("dial %s timed out", socketPath)
	return nil
}

// doHandshake performs the IPC handshake and returns the granted capabilities.
func doHandshake(t *testing.T, conn net.Conn, token string) []string {
	t.Helper()
	sendMsg(t, conn, handshakeReq{Token: token})
	resp := recvMsg(t, conn)
	if errMsg, ok := resp["error"].(string); ok {
		t.Fatalf("handshake rejected: %s", errMsg)
	}
	var caps []string
	if raw, ok := resp["caps"].([]any); ok {
		for _, c := range raw {
			caps = append(caps, c.(string))
		}
	}
	return caps
}

type testEnv struct {
	reg    *registry.Registry
	db     db.DB
	secret []byte
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	secret, err := NewSecret()
	if err != nil {
		t.Fatalf("new secret: %v", err)
	}
	return &testEnv{reg: registry.New(d), db: d, secret: secret}
}

// start creates a server with default params/input and performs the handshake.
func (e *testEnv) start(t *testing.T, params map[string]string, input any) (net.Conn, *Server) {
	t.Helper()
	return e.startWithSpec(t, params, input, nil, nil)
}

func (e *testEnv) startWithSpec(t *testing.T, params map[string]string, input any, spec *task.Spec, eng EngineRunner) (net.Conn, *Server) {
	t.Helper()
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, params, input, zap.NewNop(), spec, eng)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)
	return conn, srv
}

// ── token tests ───────────────────────────────────────────────────────────────

func TestToken_RoundTrip(t *testing.T) {
	secret, _ := NewSecret()
	tok, err := IssueToken(secret, "task:my-task", "run-1", []string{CapLog, CapParamsRead})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	claims, err := VerifyToken(secret, tok)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if claims.Identity != "task:my-task" {
		t.Errorf("identity: %q", claims.Identity)
	}
	if claims.RunID != "run-1" {
		t.Errorf("runID: %q", claims.RunID)
	}
	if !hasCap(claims.Caps, CapLog) || !hasCap(claims.Caps, CapParamsRead) {
		t.Errorf("caps missing: %v", claims.Caps)
	}
}

func TestToken_WrongSecret(t *testing.T) {
	secret, _ := NewSecret()
	other, _ := NewSecret()
	tok, _ := IssueToken(secret, "task:x", "r1", []string{CapLog})
	if _, err := VerifyToken(other, tok); err == nil {
		t.Error("expected error with wrong secret")
	}
}

func TestToken_Malformed(t *testing.T) {
	secret, _ := NewSecret()
	if _, err := VerifyToken(secret, "notavalidtoken"); err == nil {
		t.Error("expected error for malformed token")
	}
}

// ── handshake tests ───────────────────────────────────────────────────────────

func TestHandshake_InvalidToken(t *testing.T) {
	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	socketPath, _, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	defer conn.Close()

	sendMsg(t, conn, handshakeReq{Token: "bad.token"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Error("expected handshake error for invalid token")
	}
}

func TestHandshake_WrongRunID(t *testing.T) {
	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	socketPath, _, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	// Issue a token for a different run ID.
	wrongTok, _ := IssueToken(e.secret, "task:test-task", "other-run", defaultTaskCaps())

	conn := dial(t, socketPath)
	defer conn.Close()

	sendMsg(t, conn, handshakeReq{Token: wrongTok})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Error("expected handshake error for wrong run ID")
	}
}

// Regression guard: the task-channel handshake response must carry the
// taskID and runID the server was constructed with. The shim surfaces
// these as dicode.task_id / dicode.run_id, and task code (e.g. ai-agent)
// uses task_id as its self-identity for recursion guards. An empty or
// missing value silently disables those guards — see message.go for why
// the struct fields are intentionally NOT omitempty.
func TestHandshake_TaskChannelReturnsTaskAndRunID(t *testing.T) {
	e := newTestEnv(t)
	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	const taskID = "buildin/ai-agent"

	srv := New(runID, taskID, e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	defer conn.Close()

	sendMsg(t, conn, handshakeReq{Token: token})
	resp := recvMsg(t, conn)
	if errMsg, ok := resp["error"].(string); ok {
		t.Fatalf("handshake rejected: %s", errMsg)
	}

	gotTaskID, _ := resp["task_id"].(string)
	if gotTaskID != taskID {
		t.Errorf("handshake task_id: got %q, want %q", gotTaskID, taskID)
	}
	gotRunID, _ := resp["run_id"].(string)
	if gotRunID != runID {
		t.Errorf("handshake run_id: got %q, want %q", gotRunID, runID)
	}
}

// ── protocol tests ────────────────────────────────────────────────────────────

func TestServer_Params(t *testing.T) {
	e := newTestEnv(t)
	params := map[string]string{"channel": "#general", "count": "5"}
	conn, _ := e.start(t, params, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "params"})
	resp := recvMsg(t, conn)

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result not object: %v", resp["result"])
	}
	if result["channel"] != "#general" {
		t.Errorf("channel: got %v", result["channel"])
	}
}

func TestServer_Input(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, map[string]any{"msg": "hello"})

	sendMsg(t, conn, map[string]any{"id": "1", "method": "input"})
	resp := recvMsg(t, conn)

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result not object: %v", resp["result"])
	}
	if result["msg"] != "hello" {
		t.Errorf("msg: got %v", result["msg"])
	}
}

func TestServer_Input_Null(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "input"})
	resp := recvMsg(t, conn)

	if resp["result"] != nil {
		t.Errorf("expected null input, got %v", resp["result"])
	}
}

// TestServer_Log_RedactsSecretValue addresses dicode-core#126: the IPC
// `log` method must strip env-injected secret values before persisting.
// Without this, a task calling `log.info("token: " + value)` via the
// Python SDK — which wires `dicode_sdk.py:155` straight onto the `log`
// IPC method — would leak the value verbatim into the run-log table,
// bypassing the stdout/stderr redactor the runtime wrappers install.
func TestServer_Log_RedactsSecretValue(t *testing.T) {
	const secretValue = "s3cr3t-p@ssw0rd-xyz"

	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	srv.SetRedactor(secrets.NewRedactor(map[string]string{"MY_TOKEN": secretValue}))

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{
		"method":  "log",
		"level":   "info",
		"message": "secret leak attempt: " + secretValue + " trailing",
	})
	time.Sleep(20 * time.Millisecond)
	srv.Stop()

	logs, err := e.reg.GetRunLogs(context.Background(), srv.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatal("expected log entry")
	}
	if strings.Contains(logs[0].Message, secretValue) {
		t.Errorf("raw secret leaked through IPC log handler: %q", logs[0].Message)
	}
	wantMarker := "secret leak attempt: " + secrets.RedactionMarker + " trailing"
	if logs[0].Message != wantMarker {
		t.Errorf("unexpected redacted form: got %q, want %q", logs[0].Message, wantMarker)
	}
}

// TestServer_Log_NilRedactorIsPassThrough pins the nil-safe contract —
// a server with no redactor wired (existing callers, legacy tests) must
// keep logging unmodified.
func TestServer_Log_NilRedactorIsPassThrough(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	const msg = "token=abc123 (not a secret to this server)"
	sendMsg(t, conn, map[string]any{"method": "log", "level": "info", "message": msg})
	time.Sleep(20 * time.Millisecond)
	srv.Stop()

	logs, err := e.reg.GetRunLogs(context.Background(), srv.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 || logs[0].Message != msg {
		t.Errorf("nil-redactor pass-through broken: logs=%+v", logs)
	}
}

func TestServer_Log(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"method": "log", "level": "info", "message": "test message"})
	// Give the server goroutine time to receive and enqueue the message,
	// then Stop() flushes the buffer before we query.
	time.Sleep(20 * time.Millisecond)
	srv.Stop()

	logs, err := e.reg.GetRunLogs(context.Background(), srv.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatal("expected log entry")
	}
	if logs[0].Message != "test message" || logs[0].Level != "info" {
		t.Errorf("unexpected log: %+v", logs[0])
	}
}

func TestServer_Log_MultiLine(t *testing.T) {
	// Length-prefix framing correctly handles messages with embedded newlines.
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	msg := "line one\nline two\nline three"
	sendMsg(t, conn, map[string]any{"method": "log", "level": "info", "message": msg})
	time.Sleep(20 * time.Millisecond)
	srv.Stop()

	logs, _ := e.reg.GetRunLogs(context.Background(), srv.runID)
	if len(logs) == 0 {
		t.Fatal("expected log entry")
	}
	if logs[0].Message != msg {
		t.Errorf("multi-line message garbled: got %q", logs[0].Message)
	}
}

func TestServer_KV_SetGet(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"method": "kv.set", "key": "mykey", "value": map[string]any{"n": 42}})
	time.Sleep(20 * time.Millisecond)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "kv.get", "key": "mykey"})
	resp := recvMsg(t, conn)

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %T: %v", resp["result"], resp["result"])
	}
	if result["n"] != float64(42) {
		t.Errorf("expected 42, got %v", result["n"])
	}
}

func TestServer_KV_Get_Missing(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "kv.get", "key": "nokey"})
	resp := recvMsg(t, conn)

	if resp["result"] != nil {
		t.Errorf("expected null for missing key, got %v", resp["result"])
	}
}

func TestServer_KV_Delete(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"method": "kv.set", "key": "delkey", "value": "x"})
	time.Sleep(20 * time.Millisecond)
	sendMsg(t, conn, map[string]any{"method": "kv.delete", "key": "delkey"})
	time.Sleep(20 * time.Millisecond)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "kv.get", "key": "delkey"})
	resp := recvMsg(t, conn)

	if resp["result"] != nil {
		t.Errorf("expected null after delete, got %v", resp["result"])
	}
}

func TestServer_KV_Namespacing(t *testing.T) {
	// Two servers sharing the same DB must not see each other's keys.
	e := newTestEnv(t)

	makeConn := func(taskID string) net.Conn {
		runID := fmt.Sprintf("run-%s", taskID)
		srv := New(runID, taskID, e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
		sp, _, err := srv.Start(context.Background())
		if err != nil {
			t.Fatalf("Start %s: %v", taskID, err)
		}
		t.Cleanup(srv.Stop)
		tok, _ := IssueToken(e.secret, "task:"+taskID, runID, defaultTaskCaps())
		conn := dial(t, sp)
		t.Cleanup(func() { conn.Close() })
		doHandshake(t, conn, tok)
		return conn
	}

	connA := makeConn("task-a")
	connB := makeConn("task-b")

	sendMsg(t, connA, map[string]any{"method": "kv.set", "key": "shared", "value": "from-a"})
	time.Sleep(20 * time.Millisecond)

	sendMsg(t, connB, map[string]any{"id": "1", "method": "kv.get", "key": "shared"})
	resp := recvMsg(t, connB)
	if resp["result"] != nil {
		t.Errorf("task-b should not see task-a's key, got %v", resp["result"])
	}
}

func TestServer_KV_List(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	for _, k := range []string{"a", "b", "c"} {
		sendMsg(t, conn, map[string]any{"method": "kv.set", "key": k, "value": 1})
	}
	time.Sleep(30 * time.Millisecond)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "kv.list", "prefix": ""})
	resp := recvMsg(t, conn)

	keys, ok := resp["result"].([]any)
	if !ok {
		t.Fatalf("expected array, got %T: %v", resp["result"], resp["result"])
	}
	if len(keys) != 3 {
		t.Errorf("expected 3 keys, got %d: %v", len(keys), keys)
	}
}

func TestServer_Output(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{
		"method":      "output",
		"contentType": "text/html",
		"content":     "<h1>hi</h1>",
	})
	time.Sleep(20 * time.Millisecond)

	out := srv.Output()
	if out == nil {
		t.Fatal("expected output to be set")
	}
	if out.ContentType != "text/html" || out.Content != "<h1>hi</h1>" {
		t.Errorf("unexpected output: %+v", out)
	}
}

func TestServer_Return(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "return", "value": "done"})
	resp := recvMsg(t, conn)

	if resp["result"] != true {
		t.Errorf("expected true, got %v", resp["result"])
	}
	select {
	case val := <-srv.ReturnCh():
		if val != "done" {
			t.Errorf("expected 'done', got %v", val)
		}
	case <-time.After(time.Second):
		t.Fatal("returnCh timed out")
	}
}

func TestServer_Return_BeforeReply(t *testing.T) {
	// retCh must be signalled before the reply is written.
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "return", "value": 99})

	select {
	case val := <-srv.ReturnCh():
		if val != float64(99) {
			t.Errorf("expected 99, got %v", val)
		}
	case <-time.After(time.Second):
		t.Fatal("returnCh was not signalled before reply read")
	}
}

func TestServer_UnknownMethod_ReturnsError(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "no.such.method"})
	resp := recvMsg(t, conn)

	if resp["error"] == nil {
		t.Errorf("expected error for unknown method, got: %v", resp)
	}
}

// ── capability enforcement ────────────────────────────────────────────────────

func TestServer_CapDenied_KVRead(t *testing.T) {
	// Issue a token without kv.read; kv.get should be denied.
	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	socketPath, _, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	// Token with only log capability.
	tok, _ := IssueToken(e.secret, "task:test-task", runID, []string{CapLog})
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, tok)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "kv.get", "key": "x"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Error("expected permission denied for kv.get without kv.read cap")
	}
}

// ── dicode.* tests ────────────────────────────────────────────────────────────

// specWithDicode is a helper to build a spec with a DicodePermissions block.
func specWithDicode(id string, dp *task.DicodePermissions) *task.Spec {
	return &task.Spec{
		ID:          id,
		Permissions: task.Permissions{Dicode: dp},
	}
}

func TestServer_Dicode_ListTasks_Denied(t *testing.T) {
	// list_tasks is denied when permissions.dicode.list_tasks is not set.
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "hello-cron", Name: "Hello Cron"})

	conn, _ := e.start(t, nil, nil)
	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.list_tasks"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for dicode.list_tasks without list_tasks cap")
	}
}

func TestServer_Dicode_ListTasks(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "hello-cron", Name: "Hello Cron"})
	_ = e.reg.Register(&task.Spec{ID: "send-report", Name: "Send Report"})

	spec := specWithDicode("caller", &task.DicodePermissions{ListTasks: true})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)
	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.list_tasks"})
	resp := recvMsg(t, conn)

	tasks, ok := resp["result"].([]any)
	if !ok {
		t.Fatalf("expected array, got %T", resp["result"])
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestServer_Dicode_GetRuns_Denied(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.get_runs", "taskID": "some-task"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for dicode.get_runs without get_runs cap")
	}
}

func TestServer_Dicode_RunTask_Denied_NoSpec(t *testing.T) {
	// run_task is denied when no permissions.dicode block is set.
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "some-task"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for dicode.run_task without tasks cap")
	}
}

func TestServer_Dicode_RunTask_Denied_NotAllowed(t *testing.T) {
	e := newTestEnv(t)
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"permitted-task"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "forbidden-task"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected security error for unlisted task")
	}
}

func TestServer_Dicode_RunTask(t *testing.T) {
	e := newTestEnv(t)
	eng := &mockEngine{runID: "run-abc", result: RunResult{RunID: "run-abc", Status: "success"}}
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"target-task"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, eng)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "target-task"})
	resp := recvMsg(t, conn)

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", resp["result"])
	}
	if result["runID"] != "run-abc" {
		t.Errorf("runID: %v", result["runID"])
	}
}

func TestServer_Dicode_RunTask_Wildcard(t *testing.T) {
	e := newTestEnv(t)
	eng := &mockEngine{runID: "run-1", result: RunResult{RunID: "run-1", Status: "success"}}
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"*"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, eng)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "any-task"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Errorf("wildcard should allow any task, got: %v", resp["error"])
	}
}

// ── mcp.* tests ───────────────────────────────────────────────────────────────

func TestServer_MCP_Denied_NoSpec(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil) // no permissions.dicode — no mcp.call

	sendMsg(t, conn, map[string]any{"id": "1", "method": "mcp.list_tools", "mcpName": "github-mcp"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for mcp.list_tools without mcp.call cap")
	}
}

func TestServer_MCP_ListTools_NoPort(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "github-mcp"}) // MCPPort = 0

	spec := specWithDicode("caller", &task.DicodePermissions{MCP: []string{"github-mcp"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "mcp.list_tools", "mcpName": "github-mcp"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected error when mcp_port is 0")
	}
}

func TestServer_MCP_ListTools_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search","inputSchema":{"type":"object","properties":{}}}]}}`) //nolint:errcheck
	}))
	defer ts.Close()
	port := ts.Listener.Addr().(*net.TCPAddr).Port

	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "github-mcp", MCPPort: port})
	spec := specWithDicode("caller", &task.DicodePermissions{MCP: []string{"github-mcp"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "mcp.list_tools", "mcpName": "github-mcp"})
	resp := recvMsg(t, conn)

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	tools, ok := resp["result"].([]any)
	if !ok || len(tools) != 1 {
		t.Errorf("expected 1 tool, got %v", resp["result"])
	}
}

func TestServer_MCP_Call_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req["method"] == "tools/call" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"3 repos"}]}}`) //nolint:errcheck
		} else {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`) //nolint:errcheck
		}
	}))
	defer ts.Close()
	port := ts.Listener.Addr().(*net.TCPAddr).Port

	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "github-mcp", MCPPort: port})
	spec := specWithDicode("caller", &task.DicodePermissions{MCP: []string{"github-mcp"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{
		"id":      "1",
		"method":  "mcp.call",
		"mcpName": "github-mcp",
		"tool":    "search",
		"args":    map[string]any{"query": "dicode"},
	})
	resp := recvMsg(t, conn)

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if resp["result"] == nil {
		t.Error("expected non-nil result")
	}
}

func TestServer_Dicode_GetRuns(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "hello-cron", Name: "Hello Cron"})
	spec := specWithDicode("caller", &task.DicodePermissions{GetRuns: true})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.get_runs", "taskID": "hello-cron"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	// result is an array (possibly empty) — only an error field indicates failure.
}

func TestServer_MCP_Denied_WrongName(t *testing.T) {
	// Task has mcp cap for "github-mcp" but tries to call "other-mcp" — should be denied.
	e := newTestEnv(t)
	spec := specWithDicode("caller", &task.DicodePermissions{MCP: []string{"github-mcp"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "mcp.list_tools", "mcpName": "other-mcp"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for unlisted MCP daemon")
	}
}

func TestServer_MCP_Call_Denied_WrongName(t *testing.T) {
	e := newTestEnv(t)
	spec := specWithDicode("caller", &task.DicodePermissions{MCP: []string{"github-mcp"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{
		"id": "1", "method": "mcp.call", "mcpName": "other-mcp", "tool": "search",
	})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for unlisted MCP daemon")
	}
}

func TestServer_MCP_Wildcard(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`) //nolint:errcheck
	}))
	defer ts.Close()
	port := ts.Listener.Addr().(*net.TCPAddr).Port

	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "any-mcp", MCPPort: port})
	spec := specWithDicode("caller", &task.DicodePermissions{MCP: []string{"*"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "mcp.list_tools", "mcpName": "any-mcp"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Errorf("wildcard should allow any MCP daemon, got: %v", resp["error"])
	}
}

// ── dicode.secrets_* tests ────────────────────────────────────────────────────

// mockSecrets is an in-memory secrets.Manager for testing.
type mockSecrets struct {
	store map[string]string
}

func newMockSecrets() *mockSecrets { return &mockSecrets{store: map[string]string{}} }

func (m *mockSecrets) List(_ context.Context) ([]string, error) {
	keys := make([]string, 0, len(m.store))
	for k := range m.store {
		keys = append(keys, k)
	}
	return keys, nil
}
func (m *mockSecrets) Set(_ context.Context, key, value string) error {
	m.store[key] = value
	return nil
}
func (m *mockSecrets) Delete(_ context.Context, key string) error {
	delete(m.store, key)
	return nil
}

// startWithSecrets starts a server with the given spec and secrets manager wired.
func (e *testEnv) startWithSecrets(t *testing.T, spec *task.Spec, mgr *mockSecrets) (net.Conn, *Server) {
	t.Helper()
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	srv.SetSecrets(mgr)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)
	return conn, srv
}

func TestServer_Dicode_SecretsSet_Denied(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil) // no permissions.dicode.secrets_write

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets_set", "key": "FOO", "stringValue": "bar"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for dicode.secrets_set without secrets_write cap")
	}
}

func TestServer_Dicode_SecretsDelete_Denied(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets_delete", "key": "FOO"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for dicode.secrets_delete without secrets_write cap")
	}
}

func TestServer_Dicode_SecretsSet(t *testing.T) {
	e := newTestEnv(t)
	mgr := newMockSecrets()
	spec := specWithDicode("caller", &task.DicodePermissions{SecretsWrite: true})
	conn, _ := e.startWithSecrets(t, spec, mgr)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets_set", "key": "MY_TOKEN", "stringValue": "secret123"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if mgr.store["MY_TOKEN"] != "secret123" {
		t.Errorf("secret not stored: %v", mgr.store)
	}
}

func TestServer_Dicode_SecretsSet_Replace(t *testing.T) {
	e := newTestEnv(t)
	mgr := newMockSecrets()
	mgr.store["MY_TOKEN"] = "old"
	spec := specWithDicode("caller", &task.DicodePermissions{SecretsWrite: true})
	conn, _ := e.startWithSecrets(t, spec, mgr)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets_set", "key": "MY_TOKEN", "stringValue": "new"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if mgr.store["MY_TOKEN"] != "new" {
		t.Errorf("secret not replaced: got %q", mgr.store["MY_TOKEN"])
	}
}

func TestServer_Dicode_SecretsDelete(t *testing.T) {
	e := newTestEnv(t)
	mgr := newMockSecrets()
	mgr.store["MY_TOKEN"] = "secret123"
	spec := specWithDicode("caller", &task.DicodePermissions{SecretsWrite: true})
	conn, _ := e.startWithSecrets(t, spec, mgr)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets_delete", "key": "MY_TOKEN"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if _, exists := mgr.store["MY_TOKEN"]; exists {
		t.Errorf("secret not deleted")
	}
}

func TestServer_Dicode_SecretsSet_EmptyKey(t *testing.T) {
	e := newTestEnv(t)
	mgr := newMockSecrets()
	spec := specWithDicode("caller", &task.DicodePermissions{SecretsWrite: true})
	conn, _ := e.startWithSecrets(t, spec, mgr)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets_set", "stringValue": "bar"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected error for empty key")
	}
}

// ── additional coverage ───────────────────────────────────────────────────────

func TestToken_Expired(t *testing.T) {
	secret, _ := NewSecret()
	// Construct a validly signed token whose expiry is in the past.
	claims := tokenClaims{
		Identity: "task:x",
		RunID:    "r1",
		Caps:     []string{CapLog},
		Exp:      time.Now().Add(-time.Hour).Unix(),
	}
	payload, _ := json.Marshal(claims)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString(tokenSig(secret, encoded))
	tok := encoded + "." + sig

	if _, err := VerifyToken(secret, tok); err == nil {
		t.Error("expected error for expired token")
	}
}

func TestHandshake_NoHandshakeSent(t *testing.T) {
	// Connect to the server but close without sending anything. The server
	// should recover cleanly (no goroutine leak) and still accept the next
	// connection with a valid token.
	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	// First connection: connect then immediately close without sending anything.
	bad := dial(t, socketPath)
	bad.Close()

	// Brief pause so the server goroutine can process the EOF.
	time.Sleep(20 * time.Millisecond)

	// Second connection: valid handshake should still succeed.
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)
	if !hasCap(caps, CapLog) {
		t.Errorf("expected log cap after recovery; got %v", caps)
	}
}

func TestServer_CapDenied_KVWrite_Silent(t *testing.T) {
	// A fire-and-forget kv.set without the kv.write cap should be silently
	// dropped — the key must NOT appear in a subsequent kv.get.
	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	socketPath, _, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	// Token with kv.read but NOT kv.write.
	tok, _ := IssueToken(e.secret, "task:test-task", runID, []string{CapLog, CapKVRead})
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, tok)

	// Fire-and-forget kv.set (no id → no response expected).
	sendMsg(t, conn, map[string]any{"method": "kv.set", "key": "secret", "value": json.RawMessage(`"leaked"`)})

	// Give the server a moment to process the message.
	time.Sleep(20 * time.Millisecond)

	// kv.get should return null — the write was silently dropped.
	sendMsg(t, conn, map[string]any{"id": "1", "method": "kv.get", "key": "secret"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error from kv.get: %v", resp["error"])
	}
	if resp["result"] != nil {
		t.Errorf("key should not exist; got result=%v", resp["result"])
	}
}

// ── log buffering tests ───────────────────────────────────────────────────────

// TestServer_Log_FlushOnStop verifies that buffered log entries that have not
// yet been flushed by the ticker are written to the DB when Stop() is called.
func TestServer_Log_FlushOnStop(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	const n = 5
	for i := 0; i < n; i++ {
		sendMsg(t, conn, map[string]any{
			"method":  "log",
			"level":   "info",
			"message": fmt.Sprintf("msg-%d", i),
		})
	}
	// Give the server a moment to enqueue without flushing (ticker is 200ms).
	time.Sleep(10 * time.Millisecond)

	// Verify nothing in the DB yet (buffer hasn't flushed).
	logs, err := e.reg.GetRunLogs(context.Background(), srv.runID)
	if err != nil {
		t.Fatal(err)
	}
	// Entries may or may not be flushed by now (race with ticker), so we
	// only assert the final count after Stop, not the intermediate state.

	conn.Close()
	srv.Stop() // must flush

	logs, err = e.reg.GetRunLogs(context.Background(), srv.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != n {
		t.Fatalf("expected %d log entries after Stop, got %d", n, len(logs))
	}
}

// TestServer_Log_FlushOnTicker verifies that entries are flushed automatically
// after the flush interval even without an explicit Stop.
func TestServer_Log_FlushOnTicker(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)
	defer srv.Stop()
	defer conn.Close()

	sendMsg(t, conn, map[string]any{"method": "log", "level": "info", "message": "ticker-test"})

	// Wait long enough for the 200 ms ticker to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		logs, _ := e.reg.GetRunLogs(context.Background(), srv.runID)
		if len(logs) > 0 {
			return // flushed
		}
	}
	t.Fatal("log entry was not flushed within 2 s")
}

// TestServer_Log_SizeThresholdFlush verifies that when the buffer fills to
// logFlushSize (50) entries the flush happens inline before the ticker fires.
func TestServer_Log_SizeThresholdFlush(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)
	defer srv.Stop()
	defer conn.Close()

	// Send exactly logFlushSize messages. The 50th message should trigger an
	// inline flush.
	for i := 0; i < logFlushSize; i++ {
		sendMsg(t, conn, map[string]any{
			"method":  "log",
			"level":   "info",
			"message": fmt.Sprintf("bulk-%d", i),
		})
	}

	// Wait a short time (well under the 200 ms ticker) for the server to
	// process the messages and perform the size-triggered flush.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		logs, _ := e.reg.GetRunLogs(context.Background(), srv.runID)
		if len(logs) == logFlushSize {
			return // all entries written
		}
	}
	logs, _ := e.reg.GetRunLogs(context.Background(), srv.runID)
	t.Fatalf("expected %d entries after size-threshold flush, got %d", logFlushSize, len(logs))
}

// TestServer_Log_OrderingPreserved verifies that the AUTOINCREMENT rowid
// preserves insertion order across a full batch flush.
func TestServer_Log_OrderingPreserved(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	const n = 20
	for i := 0; i < n; i++ {
		sendMsg(t, conn, map[string]any{
			"method":  "log",
			"level":   "info",
			"message": fmt.Sprintf("order-%02d", i),
		})
	}
	// Give server time to receive all messages before flushing.
	time.Sleep(50 * time.Millisecond)
	srv.Stop()

	logs, err := e.reg.GetRunLogs(context.Background(), srv.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != n {
		t.Fatalf("expected %d entries, got %d", n, len(logs))
	}
	for i, lg := range logs {
		want := fmt.Sprintf("order-%02d", i)
		if lg.Message != want {
			t.Errorf("entry %d: got %q, want %q", i, lg.Message, want)
		}
	}
}

// TestServer_Log_InvalidLevel verifies that an unrecognised level value is
// normalised to "info" rather than being stored verbatim (prevents log injection).
func TestServer_Log_InvalidLevel(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{
		"method":  "log",
		"level":   "CRITICAL; DROP TABLE run_logs; --",
		"message": "injected",
	})
	// Wait for the entry to be flushed via the ticker.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		logs, _ := e.reg.GetRunLogs(context.Background(), srv.runID)
		if len(logs) > 0 {
			if logs[0].Level != "info" {
				t.Errorf("expected level normalised to \"info\", got %q", logs[0].Level)
			}
			srv.Stop()
			return
		}
	}
	srv.Stop()
	t.Fatal("log entry was not flushed within 2 s")
}

// TestServer_Log_BufCap verifies that the buffer never grows beyond
// logBufMaxSize by triggering an inline flush when the cap is hit.
func TestServer_Log_BufCap(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)
	defer srv.Stop()
	defer conn.Close()

	// Send logBufMaxSize+1 messages at once. The (logBufMaxSize+1)-th message
	// must trigger a synchronous cap-flush so all previous entries land in the
	// DB even before the 200 ms ticker fires.
	total := logBufMaxSize + 1
	for i := 0; i < total; i++ {
		sendMsg(t, conn, map[string]any{
			"method":  "log",
			"level":   "info",
			"message": fmt.Sprintf("cap-%d", i),
		})
	}

	// Wait up to 1 s for the cap-flush to land (well under the ticker).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		logs, _ := e.reg.GetRunLogs(context.Background(), srv.runID)
		if len(logs) >= logBufMaxSize {
			return // cap-flush worked
		}
	}
	logs, _ := e.reg.GetRunLogs(context.Background(), srv.runID)
	t.Fatalf("expected at least %d entries after cap-flush, got %d", logBufMaxSize, len(logs))
}

// ── secret output (issue #119) ────────────────────────────────────────────────

// TestServer_SecretOutputRoutedAndRedacted verifies that an `output` request
// with `secret: true` routes the flat map to the channel wired by
// SetSecretOutput, and persists a run-log entry with key names but a
// [redacted] placeholder rather than the raw value.
func TestServer_SecretOutputRoutedAndRedacted(t *testing.T) {
	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)

	out := make(chan map[string]string, 1)
	srv.SetSecretOutput(out)

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{
		"method":    "output",
		"secret":    true,
		"secretMap": map[string]string{"PG_URL": "postgres://x"},
	})

	select {
	case got := <-out:
		if got["PG_URL"] != "postgres://x" {
			t.Errorf("got = %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("secret map not routed to channel")
	}

	// AppendLog writes synchronously, but the IPC handler runs in a
	// separate goroutine — wait briefly for the handler to enqueue the
	// log row before reading.
	deadline := time.Now().Add(2 * time.Second)
	var logs []*registry.LogEntry
	for time.Now().Before(deadline) {
		logs, _ = e.reg.GetRunLogs(context.Background(), srv.runID)
		if len(logs) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l.Message, "[redacted]") {
			found = true
			if strings.Contains(l.Message, "postgres://x") {
				t.Errorf("plaintext leaked into log: %q", l.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected [redacted] log entry; got %d entries", len(logs))
	}
}

// TestServer_SecretOutputRejectsNestedMap verifies that a SecretMap whose
// values are objects (rather than strings) is logged-and-dropped: nothing
// arrives on the SetSecretOutput channel.
func TestServer_SecretOutputRejectsNestedMap(t *testing.T) {
	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)

	out := make(chan map[string]string, 1)
	srv.SetSecretOutput(out)

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{
		"method":    "output",
		"secret":    true,
		"secretMap": map[string]any{"PG": map[string]string{"URL": "x"}},
	})

	select {
	case got := <-out:
		t.Fatalf("nested map was accepted: %#v", got)
	case <-time.After(200 * time.Millisecond):
		// success — server logged-and-dropped.
	}
}

// TestCapRunsGetInput_NotGrantedFromYAML verifies Finding 3 (security):
// A task.Spec that previously declared permissions.dicode.runs_get_input:true
// in its YAML must NOT receive CapRunsGetInput during the IPC handshake. The
// field has been removed from DicodePermissions; this test confirms the
// cap-derivation path no longer has a YAML opt-in vector for runs.get_input.
//
// CapRunsGetInput is reserved for programmatic grant only (e.g., the
// buildin/auto-fix preset in #238). Allowing any task source to self-grant
// this capability would expose decrypted cross-task input access.
func TestCapRunsGetInput_NotGrantedFromYAML(t *testing.T) {
	e := newTestEnv(t)

	// Build a spec with every dicode permission enabled, including a
	// hypothetical runs_get_input (the field was removed from
	// DicodePermissions, so we can only set the remaining ones).
	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				RunsListExpired: true,
				RunsDeleteInput: true,
				RunsPinInput:    true,
				RunsUnpinInput:  true,
			},
		},
	}

	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	// Re-do the handshake to capture the granted caps. We can't reuse the
	// conn from startWithSpec (which already consumed the handshake), so
	// we start a fresh server and capture caps at handshake time.
	e2 := newTestEnv(t)
	runID := fmt.Sprintf("cap-test-%d", time.Now().UnixNano())
	srv := New(runID, "sec-test-task", e2.secret, e2.reg, e2.db, nil, nil, zap.NewNop(), spec, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn2 := dial(t, socketPath)
	t.Cleanup(func() { conn2.Close() })
	caps := doHandshake(t, conn2, token)

	for _, c := range caps {
		if c == CapRunsGetInput {
			t.Errorf("CapRunsGetInput granted via YAML spec — security regression; caps = %v", caps)
		}
	}

	// Sanity: the other caps must be present so we know the derivation ran.
	has := func(cap string) bool {
		for _, c := range caps {
			if c == cap {
				return true
			}
		}
		return false
	}
	if !has(CapRunsDeleteInput) {
		t.Errorf("expected CapRunsDeleteInput in caps but got %v", caps)
	}

	_ = conn // suppress unused warning from startWithSpec above
}

// TestIPC_RunsReplay_RequiresCap verifies that a task spec WITHOUT
// permissions.dicode.runs_replay: true cannot call dicode.runs.replay —
// it must receive "permission denied".
func TestIPC_RunsReplay_RequiresCap(t *testing.T) {
	e := newTestEnv(t)

	// Build a spec without runs_replay permission.
	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				RunsListExpired: true,
				RunsDeleteInput: true,
				RunsPinInput:    true,
				RunsUnpinInput:  true,
				// RunsReplay deliberately omitted.
			},
		},
	}

	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	// Send a dicode.runs.replay request — should be denied.
	sendMsg(t, conn, map[string]any{
		"id":     "replay-1",
		"method": "dicode.runs.replay",
		"runID":  "some-run-id",
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected permission denied error, got: %q (full resp: %#v)", errMsg, resp)
	}
}

// TestIPC_RunsReplay_GrantedByCap verifies that when RunsReplay is set in the
// spec, CapRunsReplay appears in the handshake capability list.
func TestIPC_RunsReplay_GrantedByCap(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				RunsReplay: true,
			},
		},
	}

	runID := fmt.Sprintf("cap-replay-test-%d", time.Now().UnixNano())
	srv := New(runID, "sec-test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)

	has := func(cap string) bool {
		for _, c := range caps {
			if c == cap {
				return true
			}
		}
		return false
	}
	if !has(CapRunsReplay) {
		t.Errorf("expected CapRunsReplay in caps when RunsReplay=true, got %v", caps)
	}
}

// TestIPC_TasksTest_RequiresCap verifies that a task spec WITHOUT
// permissions.dicode.tasks_test: true cannot call dicode.tasks.test —
// it must receive "permission denied".
func TestIPC_TasksTest_RequiresCap(t *testing.T) {
	e := newTestEnv(t)

	// Build a spec without tasks_test permission.
	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				RunsListExpired: true,
				RunsDeleteInput: true,
				RunsPinInput:    true,
				RunsUnpinInput:  true,
				// TasksTest deliberately omitted.
			},
		},
	}

	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	// Send a dicode.tasks.test request — should be denied.
	sendMsg(t, conn, map[string]any{
		"id":     "tasks-test-1",
		"method": "dicode.tasks.test",
		"taskID": "some-task-id",
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected permission denied error, got: %q (full resp: %#v)", errMsg, resp)
	}
}

// TestIPC_TasksTest_GrantedByCap verifies that when TasksTest is set in the
// spec, CapTasksTest appears in the handshake capability list.
func TestIPC_TasksTest_GrantedByCap(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				TasksTest: true,
			},
		},
	}

	runID := fmt.Sprintf("cap-tasks-test-%d", time.Now().UnixNano())
	srv := New(runID, "sec-test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)

	has := func(cap string) bool {
		for _, c := range caps {
			if c == cap {
				return true
			}
		}
		return false
	}
	if !has(CapTasksTest) {
		t.Errorf("expected CapTasksTest in caps when TasksTest=true, got %v", caps)
	}
}

// TestIPC_SourcesSetDevMode_RequiresCap verifies that a task spec WITHOUT
// permissions.dicode.sources_set_dev_mode: true cannot call
// dicode.sources.set_dev_mode — it must receive "permission denied".
func TestIPC_SourcesSetDevMode_RequiresCap(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				// SourcesSetDevMode deliberately omitted.
			},
		},
	}

	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{
		"id":     "sources-1",
		"method": "dicode.sources.set_dev_mode",
		"name":   "some-source",
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected permission denied error, got: %q (full resp: %#v)", errMsg, resp)
	}
}

// TestIPC_SourcesSetDevMode_GrantedByCap verifies that when SourcesSetDevMode
// is set in the spec, CapSourcesSetDevMode appears in the handshake caps.
func TestIPC_SourcesSetDevMode_GrantedByCap(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				SourcesSetDevMode: true,
			},
		},
	}

	runID := fmt.Sprintf("cap-sources-set-dev-mode-%d", time.Now().UnixNano())
	srv := New(runID, "sec-test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)

	has := func(cap string) bool {
		for _, c := range caps {
			if c == cap {
				return true
			}
		}
		return false
	}
	if !has(CapSourcesSetDevMode) {
		t.Errorf("expected CapSourcesSetDevMode in caps when SourcesSetDevMode=true, got %v", caps)
	}
}

// TestIPC_GitCommitPush_RequiresCap verifies that a task spec WITHOUT
// permissions.dicode.git_commit_push: true cannot call
// dicode.git.commit_push — it must receive "permission denied".
func TestIPC_GitCommitPush_RequiresCap(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				// GitCommitPush deliberately omitted.
			},
		},
	}

	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{
		"id":        "git-1",
		"method":    "dicode.git.commit_push",
		"source_id": "some-source",
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected permission denied error, got: %q (full resp: %#v)", errMsg, resp)
	}
}

// TestIPC_GitCommitPush_GrantedByCap verifies that when GitCommitPush is set
// in the spec, CapGitCommitPush appears in the handshake caps, and that
// calling the method with no source_id returns "source_id required"
// (proves we got past the cap gate).
func TestIPC_GitCommitPush_GrantedByCap(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				GitCommitPush: true,
			},
		},
	}

	runID := fmt.Sprintf("cap-git-commit-push-%d", time.Now().UnixNano())
	srv := New(runID, "sec-test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)

	has := func(cap string) bool {
		for _, c := range caps {
			if c == cap {
				return true
			}
		}
		return false
	}
	if !has(CapGitCommitPush) {
		t.Errorf("expected CapGitCommitPush in caps when GitCommitPush=true, got %v", caps)
	}

	// Calling with no source_id must fail with "source_id required" (past cap gate).
	sendMsg(t, conn, map[string]any{
		"id":     "git-2",
		"method": "dicode.git.commit_push",
		// source_id intentionally omitted
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "source_id required") {
		t.Errorf("expected source_id required error, got: %q (full resp: %#v)", errMsg, resp)
	}
}
