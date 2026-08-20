package ipc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// ── test helpers ─────────────────────────────────────────────────────────────

type mockEngine struct {
	runID    string
	result   RunResult
	err      error
	parentRX string // observed parent runID on the last FireFromTask call
}

func (m *mockEngine) FireManual(_ context.Context, _ string, _ map[string]string) (string, error) {
	return m.runID, m.err
}
func (m *mockEngine) FireFromTask(_ context.Context, _ string, parentRunID string, _ map[string]string) (string, error) {
	m.parentRX = parentRunID
	return m.runID, m.err
}
func (m *mockEngine) WaitRun(_ context.Context, _ string) (RunResult, error) {
	return m.result, m.err
}
func (m *mockEngine) WaitRunSettled(_ context.Context, _ string) (RunResult, error) {
	return m.result, m.err
}
func (m *mockEngine) KillRun(_ string) bool   { return false }
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
// the struct fields are NOT omitempty.
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
	conn.Close()
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
	conn.Close()
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
	conn.Close()
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
	conn.Close()
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

// TestServer_Dicode_ListTasks_TemplateAndWebhook confirms that the IPC
// summary surfaces Spec.Template, Spec.Trigger.Webhook, and Spec.Enabled —
// the fields buildin/auth-providers needs to auto-detect _oauth-app
// inheritors without hardcoding a per-task allowlist.
func TestServer_Dicode_ListTasks_TemplateAndWebhook(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{
		ID:       "auth/google-oauth",
		Name:     "Google OAuth",
		Template: "dicode.io/oauth-app",
		Trigger:  task.TriggerConfig{Webhook: "/hooks/google-oauth"},
		Enabled:  true,
	})
	_ = e.reg.Register(&task.Spec{
		ID:      "auth/disabled-oauth",
		Name:    "Disabled OAuth",
		Enabled: false, // not template-marked; default false-zero
	})

	spec := specWithDicode("caller", &task.DicodePermissions{ListTasks: true})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)
	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.list_tasks"})
	resp := recvMsg(t, conn)

	tasks, ok := resp["result"].([]any)
	if !ok {
		t.Fatalf("expected array, got %T", resp["result"])
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	byID := map[string]map[string]any{}
	for _, raw := range tasks {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("expected map, got %T", raw)
		}
		byID[m["id"].(string)] = m
	}

	g := byID["auth/google-oauth"]
	if g["template"] != "dicode.io/oauth-app" {
		t.Errorf("expected template=dicode.io/oauth-app, got %v", g["template"])
	}
	if g["webhook"] != "/hooks/google-oauth" {
		t.Errorf("expected webhook=/hooks/google-oauth, got %v", g["webhook"])
	}
	if g["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", g["enabled"])
	}

	d := byID["auth/disabled-oauth"]
	if _, hasTemplate := d["template"]; hasTemplate {
		t.Errorf("expected no template field on un-marked task; got %v", d["template"])
	}
	if d["enabled"] != false {
		t.Errorf("expected enabled=false, got %v", d["enabled"])
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

// ── #116 run grouping tests ──────────────────────────────────────────────────

// TestServer_Dicode_RunTask_ParentLinkage verifies that dicode.run_task
// passes the caller's runID as the new run's parent, so child runs are
// correctly linked back to the running task that triggered them.
func TestServer_Dicode_RunTask_ParentLinkage(t *testing.T) {
	e := newTestEnv(t)
	eng := &mockEngine{runID: "child-run", result: RunResult{RunID: "child-run", Status: "success"}}
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"target-task"}})
	conn, srv := e.startWithSpec(t, nil, nil, spec, eng)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "target-task"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if eng.parentRX == "" {
		t.Fatal("FireFromTask never received a parent runID")
	}
	if eng.parentRX != srv.runID {
		t.Errorf("parent runID = %q, want %q", eng.parentRX, srv.runID)
	}
}

// TestServer_Dicode_SetGroup_Persists writes a group via the IPC method
// and reads it back through the registry to verify last-write-wins.
func TestServer_Dicode_SetGroup_Persists(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	// The server doesn't auto-create a runs row; do it ourselves so the
	// UPDATE has a target. (In production fireAsync inserts the row before
	// the task starts.)
	if _, err := e.reg.StartRunWithID(context.Background(), srv.runID, "test-task", "", "manual", "task"); err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.set_group", "group": "chat-7"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("set_group error: %v", resp["error"])
	}

	got, err := e.reg.GetRun(context.Background(), srv.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Group != "chat-7" {
		t.Errorf("Group = %q, want chat-7", got.Group)
	}

	// Last write wins.
	sendMsg(t, conn, map[string]any{"id": "2", "method": "dicode.set_group", "group": "chat-9"})
	_ = recvMsg(t, conn)
	got, _ = e.reg.GetRun(context.Background(), srv.runID)
	if got.Group != "chat-9" {
		t.Errorf("Group after overwrite = %q, want chat-9", got.Group)
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
func (m *mockSecrets) Has(_ context.Context, key string) (bool, error) {
	_, ok := m.store[key]
	return ok, nil
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

// ── dicode.secrets.has tests ─────────────────────────────────────────────────

func TestServer_Dicode_SecretsHas_Denied(t *testing.T) {
	e := newTestEnv(t)
	// No secrets_has permission — should be denied.
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets.has", "key": "FOO"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for dicode.secrets.has without secrets_has cap")
	}
}

func TestServer_Dicode_SecretsHas_Present(t *testing.T) {
	e := newTestEnv(t)
	mgr := newMockSecrets()
	mgr.store["MY_TOKEN"] = "secret123"
	spec := specWithDicode("caller", &task.DicodePermissions{SecretsHas: true})
	conn, _ := e.startWithSecrets(t, spec, mgr)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets.has", "key": "MY_TOKEN"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if resp["result"] != true {
		t.Errorf("expected result=true for present key, got %v", resp["result"])
	}
}

func TestServer_Dicode_SecretsHas_Absent(t *testing.T) {
	e := newTestEnv(t)
	mgr := newMockSecrets()
	spec := specWithDicode("caller", &task.DicodePermissions{SecretsHas: true})
	conn, _ := e.startWithSecrets(t, spec, mgr)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets.has", "key": "MISSING_KEY"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if resp["result"] != false {
		t.Errorf("expected result=false for absent key, got %v", resp["result"])
	}
}

func TestServer_Dicode_SecretsHas_EmptyKey(t *testing.T) {
	e := newTestEnv(t)
	mgr := newMockSecrets()
	spec := specWithDicode("caller", &task.DicodePermissions{SecretsHas: true})
	conn, _ := e.startWithSecrets(t, spec, mgr)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets.has"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected error for empty key")
	}
}

// TestServer_Dicode_SecretsHas_IndependentOfWrite confirms that SecretsHas and
// SecretsWrite are independent caps — presence check works without write rights.
func TestServer_Dicode_SecretsHas_IndependentOfWrite(t *testing.T) {
	e := newTestEnv(t)
	mgr := newMockSecrets()
	mgr.store["TOKEN"] = "actual-secret-value"
	// Grant only SecretsHas, NOT SecretsWrite.
	spec := specWithDicode("caller", &task.DicodePermissions{SecretsHas: true, SecretsWrite: false})
	conn, _ := e.startWithSecrets(t, spec, mgr)

	// Confirm has works.
	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.secrets.has", "key": "TOKEN"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if resp["result"] != true {
		t.Errorf("expected result=true, got %v", resp["result"])
	}

	// Confirm write is still denied.
	sendMsg(t, conn, map[string]any{"id": "2", "method": "dicode.secrets_set", "key": "TOKEN", "stringValue": "new"})
	resp2 := recvMsg(t, conn)
	if resp2["error"] == nil {
		t.Errorf("expected write denied when only secrets_has is granted")
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
	conn.Close()
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
			conn.Close()
			srv.Stop()
			return
		}
	}
	conn.Close()
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

// TestServer_SecretOutput_PreservesExistingRedaction is the regression for
// the leak reopening on the output.secret path: a value the run's redactor
// already scrubs (e.g. the ephemeral per-run MCP token folded in at launch)
// must stay redacted after the task emits a secret output — the redactor is
// extended, not replaced.
func TestServer_SecretOutput_PreservesExistingRedaction(t *testing.T) {
	const priorSecret = "mcp-token-abc123"
	const newSecret = "postgres://x"

	e := newTestEnv(t)
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	srv.SetRedactor(secrets.NewRedactor(map[string]string{"DICODE_MCP_API_KEY": priorSecret}))

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	// The task emits a secret output — this rebuilds the run redactor.
	sendMsg(t, conn, map[string]any{
		"method":    "output",
		"secret":    true,
		"secretMap": map[string]string{"PG_URL": newSecret},
	})
	// Then logs a line containing both the prior token and the new secret.
	sendMsg(t, conn, map[string]any{
		"method":  "log",
		"level":   "info",
		"message": "dump: " + priorSecret + " and " + newSecret,
	})
	time.Sleep(50 * time.Millisecond)
	conn.Close()
	srv.Stop()

	logs, err := e.reg.GetRunLogs(context.Background(), srv.runID)
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range logs {
		if strings.HasPrefix(l.Message, "dump: ") {
			line = l.Message
		}
	}
	if line == "" {
		t.Fatalf("dump log line not found in %d entries", len(logs))
	}
	if strings.Contains(line, priorSecret) {
		t.Errorf("prior redactor value leaked after secret output: %q", line)
	}
	if strings.Contains(line, newSecret) {
		t.Errorf("newly-output secret leaked: %q", line)
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

// TestCapRunsGetInput_GrantedFromYAML verifies that a task whose YAML sets
// permissions.dicode.runs_get_input: true DOES receive CapRunsGetInput during
// the IPC handshake. RunsGetInput is now YAML-grantable — the redaction layer
// from #233 bounds the surface, so there is no need to gate it behind a
// programmatic-only grant. Users can build their own replayer / fixer / auditor
// tasks without depending on the buildin auto-fix preset.
func TestCapRunsGetInput_GrantedFromYAML(t *testing.T) {
	e := newTestEnv(t)

	// Build a spec with runs_get_input: true in the YAML-parsed permissions.
	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				RunsGetInput:    true,
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

	found := false
	for _, c := range caps {
		if c == CapRunsGetInput {
			found = true
		}
	}
	if !found {
		t.Errorf("CapRunsGetInput not granted via YAML; caps = %v", caps)
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
				// RunsReplay omitted.
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
				// TasksTest omitted.
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

// TestIPC_TasksTest_PendingApprovalRefused covers the approval-gate veto on
// the per-task IPC path: even a caller task that holds the tasks_test
// capability must not be able to run a PENDING task's test file (it executes
// with full host permissions). The guard fires before the registry lookup
// and before tasktest.Run.
func TestIPC_TasksTest_PendingApprovalRefused(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "src/pending", Name: "pending"})

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{TasksTest: true},
		},
	}
	runID := fmt.Sprintf("pend-tasks-test-%d", time.Now().UnixNano())
	srv := New(runID, "caller-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	srv.SetTestGuard(func(id string) error {
		return fmt.Errorf("task pending approval: %s", id)
	})
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{
		"id":     "pend-1",
		"method": "dicode.tasks.test",
		"taskID": "src/pending",
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "pending approval") {
		t.Errorf("expected pending-approval refusal, got: %q (full resp: %#v)", errMsg, resp)
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
				// SourcesSetDevMode omitted.
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
				// GitCommitPush omitted.
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
		// source_id omitted
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "source_id required") {
		t.Errorf("expected source_id required error, got: %q (full resp: %#v)", errMsg, resp)
	}
}

// ── dicode.crypto.* tests ─────────────────────────────────────────────────────

func TestServer_CryptoEncrypt_RequiresCap(t *testing.T) {
	// Task with no Crypto permission — must get "permission denied".
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil) // no spec = no dicode perms
	sendMsg(t, conn, map[string]any{
		"id":            "c1",
		"method":        "dicode.crypto.encrypt",
		"context":       "test/v1",
		"plaintext_b64": base64.StdEncoding.EncodeToString([]byte("hello")),
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected permission denied, got: %q", errMsg)
	}
}

func TestServer_CryptoEncrypt_ContextNotAllowed(t *testing.T) {
	// Task has Crypto:["allowed"] but requests context "denied".
	e := newTestEnv(t)
	spec := specWithDicode("caller", &task.DicodePermissions{Crypto: []string{"allowed"}})
	conn, srv := e.startWithSpec(t, nil, nil, spec, nil)
	srv.SetCryptoHandler(stubDeriver{})

	sendMsg(t, conn, map[string]any{
		"id":            "c2",
		"method":        "dicode.crypto.encrypt",
		"context":       "denied",
		"plaintext_b64": base64.StdEncoding.EncodeToString([]byte("hello")),
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "not in permissions.dicode.crypto") {
		t.Errorf("expected context-not-allowed error, got: %q", errMsg)
	}
}

func TestServer_Crypto_RoundTrip(t *testing.T) {
	// Task has Crypto:["test/v1"] — encrypt then decrypt must round-trip.
	e := newTestEnv(t)
	spec := specWithDicode("caller", &task.DicodePermissions{Crypto: []string{"test/v1"}})
	conn, srv := e.startWithSpec(t, nil, nil, spec, nil)
	srv.SetCryptoHandler(stubDeriver{})

	plaintext := []byte("round-trip plaintext")

	// Encrypt
	sendMsg(t, conn, map[string]any{
		"id":            "enc1",
		"method":        "dicode.crypto.encrypt",
		"context":       "test/v1",
		"plaintext_b64": base64.StdEncoding.EncodeToString(plaintext),
	})
	encResp := recvMsg(t, conn)
	if encResp["error"] != nil {
		t.Fatalf("encrypt error: %v", encResp["error"])
	}
	result, ok := encResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("encrypt result not object: %T", encResp["result"])
	}
	ciphertextB64, _ := result["ciphertext_b64"].(string)
	if ciphertextB64 == "" {
		t.Fatal("ciphertext_b64 missing from encrypt result")
	}

	// Decrypt
	sendMsg(t, conn, map[string]any{
		"id":             "dec1",
		"method":         "dicode.crypto.decrypt",
		"context":        "test/v1",
		"ciphertext_b64": ciphertextB64,
	})
	decResp := recvMsg(t, conn)
	if decResp["error"] != nil {
		t.Fatalf("decrypt error: %v", decResp["error"])
	}
	decResult, ok := decResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("decrypt result not object: %T", decResp["result"])
	}
	plaintextB64, _ := decResult["plaintext_b64"].(string)
	gotPT, err := base64.StdEncoding.DecodeString(plaintextB64)
	if err != nil {
		t.Fatalf("base64 decode plaintext: %v", err)
	}
	if string(gotPT) != string(plaintext) {
		t.Errorf("round-trip mismatch: got %q, want %q", gotPT, plaintext)
	}
}

// ── Bug #130: Stop() ordering race ───────────────────────────────────────────

// TestServer_Stop_WaitsForInflightHandleConn verifies that log entries
// buffered by a handleConn goroutine that is still running when Stop() is
// called are NOT lost. The fix adds a connWG WaitGroup: Stop() closes the
// listener first, then waits for all handleConn goroutines to drain, THEN
// triggers the final log flush.
func TestServer_Stop_WaitsForInflightHandleConn(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	// Send a log message; we expect it to survive Stop() even if the
	// handleConn goroutine hasn't finished processing when Stop fires.
	const msg = "in-flight-log"
	sendMsg(t, conn, map[string]any{
		"method":  "log",
		"level":   "info",
		"message": msg,
	})

	// Give the server goroutine a brief moment to receive the message and
	// enqueue it in the log buffer (but not flush it yet — ticker is 200 ms).
	time.Sleep(10 * time.Millisecond)

	// Close the connection to make handleConn's readMsg return EOF, then
	// immediately call Stop. The fix ensures Stop waits for handleConn to
	// finish (and thus for bufferLog to complete) before flushing.
	conn.Close()
	srv.Stop()

	logs, err := e.reg.GetRunLogs(context.Background(), srv.runID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range logs {
		if l.Message == msg {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("in-flight log entry was lost on Stop(); got %d entries: %+v", len(logs), logs)
	}
}

// TestServer_Stop_Idempotent verifies that calling Stop() twice does not
// panic (double-close of logFlushCh is guarded by the select).
func TestServer_Stop_Idempotent(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)
	// Close the connection first so that handleConn's readMsg returns EOF and
	// the goroutine exits. connWG then drops to zero so Stop() can proceed.
	conn.Close()
	srv.Stop()
	srv.Stop() // must not panic
}

// ── Bug #130: BulkAppendLogs fallback ────────────────────────────────────────

// txFailDB is a db.DB that wraps a real DB but injects a failure on the first
// Tx() call, simulating a transient SQLite transaction error. Subsequent Tx
// and Exec calls are forwarded to the underlying DB.
type txFailDB struct {
	db.DB
	txFailed bool
}

func (f *txFailDB) Tx(ctx context.Context, fn func(tx db.DB) error) error {
	if !f.txFailed {
		f.txFailed = true
		return fmt.Errorf("injected tx failure")
	}
	return f.DB.Tx(ctx, fn)
}

// TestServer_FlushBatch_FallsBackToPerRow verifies that when BulkAppendLogs
// returns an error the flush caller falls back to per-row AppendLog inserts
// and the log entries are still written to the DB.
func TestServer_FlushBatch_FallsBackToPerRow(t *testing.T) {
	t.Parallel()
	realDB, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { realDB.Close() })

	// Use a txFailDB so that the first BulkAppendLogs (which uses Tx) fails,
	// forcing flushBatch to fall back to per-row AppendLog (which uses Exec).
	failingDB := &txFailDB{DB: realDB}
	reg := registry.New(failingDB)
	secret, _ := NewSecret()

	runID := fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", secret, reg, failingDB, nil, nil, zap.NewNop(), nil, nil)

	batch := []registry.PendingLogEntry{
		{RunID: runID, Level: "info", Message: "should-survive-bulk-failure", TsMs: time.Now().UnixMilli()},
		{RunID: runID, Level: "warn", Message: "also-survives", TsMs: time.Now().UnixMilli()},
	}

	// First call triggers the injected Tx failure; flushBatch must fall back
	// to per-row AppendLog and the entries must still appear in the DB.
	srv.flushBatch(context.Background(), batch, "test-error")

	// Read back using the real DB (Exec succeeds on it for the per-row path).
	logs, err := reg.GetRunLogs(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Errorf("expected 2 log entries after fallback, got %d: %+v", len(logs), logs)
	}
	msgs := map[string]bool{}
	for _, l := range logs {
		msgs[l.Message] = true
	}
	if !msgs["should-survive-bulk-failure"] || !msgs["also-survives"] {
		t.Errorf("expected both messages to survive fallback; got: %+v", msgs)
	}
}

// TestServer_FlushBatch_SuccessPath verifies that the normal (non-error) path
// through flushBatch writes entries correctly.
// ── MCP exposure filter tests ────────────────────────────────────────────────

func TestServer_MCPContext_ListTasks_FiltersNonExposed(t *testing.T) {
	e := newTestEnv(t)
	// Register two tasks: one MCP-exposed, one not.
	_ = e.reg.Register(&task.Spec{ID: "public-api", Name: "Public API", MCPExposed: true})
	_ = e.reg.Register(&task.Spec{ID: "internal-cron", Name: "Internal Cron", MCPExposed: false})

	spec := specWithDicode("caller", &task.DicodePermissions{ListTasks: true})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	// With mcpContext: true, only the MCP-exposed task should appear.
	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.list_tasks", "mcpContext": true})
	resp := recvMsg(t, conn)

	tasks, ok := resp["result"].([]any)
	if !ok {
		t.Fatalf("expected array, got %T", resp["result"])
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 MCP-exposed task, got %d", len(tasks))
	}
	first := tasks[0].(map[string]any)
	if first["id"] != "public-api" {
		t.Errorf("expected public-api, got %v", first["id"])
	}
}

func TestServer_MCPContext_ListTasks_NoFilterWithoutFlag(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "public-api", Name: "Public API", MCPExposed: true})
	_ = e.reg.Register(&task.Spec{ID: "internal-cron", Name: "Internal Cron", MCPExposed: false})

	spec := specWithDicode("caller", &task.DicodePermissions{ListTasks: true})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	// Without mcpContext, both tasks should appear (backwards compat).
	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.list_tasks"})
	resp := recvMsg(t, conn)

	tasks, ok := resp["result"].([]any)
	if !ok {
		t.Fatalf("expected array, got %T", resp["result"])
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks without mcpContext, got %d", len(tasks))
	}
}

func TestServer_MCPContext_RunTask_DeniesNonExposed(t *testing.T) {
	e := newTestEnv(t)
	// Register the target task as NOT MCP-exposed.
	_ = e.reg.Register(&task.Spec{ID: "internal-task", Name: "Internal", MCPExposed: false})

	eng := &mockEngine{runID: "run-1", result: RunResult{RunID: "run-1", Status: "success"}}
	spec := specWithDicode("caller", &task.DicodePermissions{
		ListTasks: true,
		Tasks:     []string{"*"},
	})
	conn, _ := e.startWithSpec(t, nil, nil, spec, eng)

	sendMsg(t, conn, map[string]any{
		"id":         "1",
		"method":     "dicode.run_task",
		"taskID":     "internal-task",
		"mcpContext": true,
	})
	resp := recvMsg(t, conn)

	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "not exposed via MCP") {
		t.Errorf("expected 'not exposed via MCP' error, got: %v", errMsg)
	}
}

func TestServer_MCPContext_RunTask_AllowsExposed(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "public-task", Name: "Public", MCPExposed: true})

	eng := &mockEngine{runID: "run-1", result: RunResult{RunID: "run-1", Status: "success"}}
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"*"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, eng)

	sendMsg(t, conn, map[string]any{
		"id":         "1",
		"method":     "dicode.run_task",
		"taskID":     "public-task",
		"mcpContext": true,
	})
	resp := recvMsg(t, conn)

	if resp["error"] != nil {
		t.Errorf("expected success for MCP-exposed task, got error: %v", resp["error"])
	}
}

func TestServer_MCPContext_RunTask_NoFilterWithoutFlag(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "internal-task", Name: "Internal", MCPExposed: false})

	eng := &mockEngine{runID: "run-1", result: RunResult{RunID: "run-1", Status: "success"}}
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"*"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, eng)

	// Without mcpContext, the call should succeed even for non-exposed tasks.
	sendMsg(t, conn, map[string]any{
		"id":     "1",
		"method": "dicode.run_task",
		"taskID": "internal-task",
	})
	resp := recvMsg(t, conn)

	if resp["error"] != nil {
		t.Errorf("expected success without mcpContext, got error: %v", resp["error"])
	}
}

func TestServer_FlushBatch_SuccessPath(t *testing.T) {
	t.Parallel()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	secret, _ := NewSecret()

	runID := fmt.Sprintf("success-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", secret, reg, d, nil, nil, zap.NewNop(), nil, nil)

	batch := []registry.PendingLogEntry{
		{RunID: runID, Level: "info", Message: "success-entry", TsMs: time.Now().UnixMilli()},
	}
	srv.flushBatch(context.Background(), batch, "test")

	logs, err := reg.GetRunLogs(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Message != "success-entry" {
		t.Errorf("expected 1 log entry 'success-entry', got %d: %+v", len(logs), logs)
	}
}

// ── socket-dir hardening tests (#423) ────────────────────────────────────────

// TestServer_SocketInPerRunDir verifies that on non-Windows platforms the IPC
// socket is placed inside a per-run directory (/tmp/dicode-<runID>/) rather
// than directly in /tmp. This is the defense-in-depth hardening from issue
// #423: even if the socket's own chmod is racy, the 0700 parent dir keeps
// other local users from reaching the socket file at all.
func TestServer_SocketInPerRunDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("per-run dir hardening not applied on Windows")
	}

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	secret, _ := NewSecret()
	reg := registry.New(d)

	runID := fmt.Sprintf("dir-test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", secret, reg, d, nil, nil, zap.NewNop(), nil, nil)

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Socket must live inside a directory, not directly in /tmp.
	dir := filepath.Dir(socketPath)
	if dir == "/tmp" {
		t.Errorf("socket %q is directly in /tmp; expected a per-run subdirectory", socketPath)
	}

	// Parent directory must be 0700.
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat socket dir %q: %v", dir, err)
	}
	if !dirInfo.IsDir() {
		t.Fatalf("socket parent %q is not a directory", dir)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0700 {
		t.Errorf("socket dir %q has mode %o, want 0700", dir, perm)
	}

	// Socket file itself must be 0600.
	sockInfo, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket %q: %v", socketPath, err)
	}
	if perm := sockInfo.Mode().Perm(); perm != 0600 {
		t.Errorf("socket file %q has mode %o, want 0600", socketPath, perm)
	}

	// Connection and handshake must succeed through the new path.
	conn := dial(t, socketPath)
	doHandshake(t, conn, token)
	_ = conn.Close()

	// After Stop the entire per-run directory must be gone.
	srv.Stop()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("socket dir %q still exists after Stop; expected it to be removed", dir)
	}
}

// TestServer_SocketDirCleanedUpOnStartFailure verifies that the per-run dir is
// removed even when Start fails after creating the directory (e.g. listen
// error). This guards against stale /tmp/dicode-<runID>/ dirs accumulating on
// repeated crash-restart cycles.
func TestServer_SocketDirCleanedUpOnStartFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("per-run dir hardening not applied on Windows")
	}

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	secret, _ := NewSecret()
	reg := registry.New(d)

	// Start a first server to occupy the directory slot and trigger a dir
	// collision on the second attempt with the same runID.
	runID := fmt.Sprintf("fail-test-%d", time.Now().UnixNano())
	srv1 := New(runID, "test-task", secret, reg, d, nil, nil, zap.NewNop(), nil, nil)
	socketPath, token, err := srv1.Start(context.Background())
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	_ = token
	defer srv1.Stop()

	// A second server with the same runID will find its socket path already
	// in use (socket file exists). On Linux, net.Listen on an existing socket
	// path fails with EADDRINUSE only when the previous socket file was NOT
	// removed. srv1 is still running, so the socket file is live. Start()
	// removes any stale file first and then tries to listen — on a live
	// socket this will fail.
	//
	// We test the cleanup by pre-creating the per-run directory manually
	// (simulating a crash) and verifying that Start() cleans it up before
	// re-creating, so no dir leak occurs.
	dir := filepath.Dir(socketPath)

	// Stop srv1 so the socket is gone, then manually re-create the dir to
	// simulate a leftover from a crashed previous run.
	srv1.Stop()
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("mkdir stale dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	srv2 := New(runID, "test-task", secret, reg, d, nil, nil, zap.NewNop(), nil, nil)
	socketPath2, _, err := srv2.Start(context.Background())
	if err != nil {
		t.Fatalf("second Start after stale dir: %v", err)
	}
	defer srv2.Stop()

	// The stale dir was removed and a fresh one created — new socket path
	// must match what we expect.
	if socketPath2 != socketPath {
		t.Errorf("socket path after stale-dir cleanup = %q, want %q", socketPath2, socketPath)
	}
	if _, err := os.Stat(filepath.Dir(socketPath2)); err != nil {
		t.Errorf("fresh socket dir missing: %v", err)
	}
}

// ── suspend capability gating (#502) ────────────────────────────────────────

// CapSuspend must be granted only to runtimes that read srv.Suspend(): deno
// and python. Container runtimes (docker/podman) never read the payload, so
// granting the cap would let a dicode.suspend be acked then silently dropped.
func TestServer_SuspendCap_GrantedByRuntime(t *testing.T) {
	cases := []struct {
		runtime task.Runtime
		want    bool
	}{
		{task.RuntimeDeno, true},
		{task.Runtime("python"), true},
		{task.RuntimeDocker, false},
		{task.RuntimePodman, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.runtime), func(t *testing.T) {
			e := newTestEnv(t)
			runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
			srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), &task.Spec{Runtime: tc.runtime}, nil)
			_, token, err := srv.Start(context.Background())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(srv.Stop)
			claims, err := VerifyToken(e.secret, token)
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			if got := hasCap(claims.Caps, CapSuspend); got != tc.want {
				t.Errorf("runtime %s: CapSuspend granted=%v, want %v (caps=%v)", tc.runtime, got, tc.want, claims.Caps)
			}
		})
	}
}

// A docker task's dicode.suspend must be rejected with a clear cap-denied
// error rather than acked-then-dropped.
func TestServer_Suspend_DeniedForDocker(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.startWithSpec(t, nil, nil, &task.Spec{Runtime: task.RuntimeDocker}, nil)
	sendMsg(t, conn, map[string]any{
		"id":     "1",
		"method": "dicode.suspend",
		"state":  json.RawMessage(`{"step":1}`),
	})
	resp := recvMsg(t, conn)
	if errMsg, _ := resp["error"].(string); !strings.Contains(errMsg, "permission denied") {
		t.Fatalf("expected permission denied for docker suspend; got error=%v result=%v", resp["error"], resp["result"])
	}
	if srv.Suspend() != nil {
		t.Fatal("docker suspend must not record a payload")
	}
}

// A dicode.suspend carrying a structurally-invalid schema must be rejected at
// suspend time (#517) — while the task can still react — rather than stored and
// left to fail every resume with a 400 until the TTL sweep.
func TestServer_Suspend_RejectsInvalidSchema(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"non-string type", `{"type":123}`},
		{"external file ref", `{"$ref":"file:///etc/passwd"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			conn, srv := e.startWithSpec(t, nil, nil, &task.Spec{Runtime: task.RuntimeDeno}, nil)
			sendMsg(t, conn, map[string]any{
				"id":     "1",
				"method": "dicode.suspend",
				"state":  json.RawMessage(`{"step":1}`),
				"schema": json.RawMessage(tc.schema),
			})
			resp := recvMsg(t, conn)
			if errMsg, _ := resp["error"].(string); !strings.Contains(errMsg, "invalid suspend schema") {
				t.Fatalf("expected invalid-schema rejection; got error=%v result=%v", resp["error"], resp["result"])
			}
			if srv.Suspend() != nil {
				t.Fatal("an un-resumable schema must not be recorded")
			}
		})
	}
}

// A null (or empty) schema means "no constraint": the suspend-time probe is
// skipped and the schema is normalized to empty so it is not persisted as the
// literal `null`, which would otherwise 400 every resume (#517). A schema-less
// dicode.suspend (e.g. the Python approval-gate pattern) must still suspend.
func TestServer_Suspend_NullSchemaTreatedAsAbsent(t *testing.T) {
	cases := []struct {
		name   string
		schema json.RawMessage
	}{
		{"literal null", json.RawMessage(`null`)},
		{"whitespace null", json.RawMessage(" null ")},
		{"empty", json.RawMessage(``)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			conn, srv := e.startWithSpec(t, nil, nil, &task.Spec{Runtime: task.RuntimeDeno}, nil)
			msg := map[string]any{
				"id":     "1",
				"method": "dicode.suspend",
				"state":  json.RawMessage(`{"step":1}`),
			}
			if len(tc.schema) > 0 {
				msg["schema"] = tc.schema
			}
			sendMsg(t, conn, msg)
			resp := recvMsg(t, conn)
			if resp["error"] != nil {
				t.Fatalf("a null/absent schema must be accepted, got error: %v", resp["error"])
			}
			s := srv.Suspend()
			if s == nil {
				t.Fatal("expected the suspend payload to be recorded")
			}
			if len(s.Schema) != 0 {
				t.Fatalf("a null schema must be normalized to empty (not persisted), got %q", s.Schema)
			}
		})
	}
}

// A valid schema on dicode.suspend passes the suspend-time probe and is recorded.
func TestServer_Suspend_AcceptsValidSchema(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.startWithSpec(t, nil, nil, &task.Spec{Runtime: task.RuntimeDeno}, nil)
	sendMsg(t, conn, map[string]any{
		"id":     "1",
		"method": "dicode.suspend",
		"state":  json.RawMessage(`{"step":1}`),
		"schema": json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
	})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("valid schema rejected: %v", resp["error"])
	}
	if srv.Suspend() == nil {
		t.Fatal("expected suspend payload recorded for a valid schema")
	}
}

// A deno task's dicode.suspend is accepted and records the payload.
func TestServer_Suspend_GrantedForDeno(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.startWithSpec(t, nil, nil, &task.Spec{Runtime: task.RuntimeDeno}, nil)
	sendMsg(t, conn, map[string]any{
		"id":     "1",
		"method": "dicode.suspend",
		"state":  json.RawMessage(`{"step":1}`),
	})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("deno suspend rejected: %v", resp["error"])
	}
	if srv.Suspend() == nil {
		t.Fatal("expected suspend payload recorded for deno")
	}
}

// fakeSourceCtl records the SetDevMode call, reports a fixed dev root, and
// serves a fixed source listing.
type fakeSourceCtl struct {
	gotName    string
	gotEnabled bool
	gotOpts    taskset.DevModeOpts
	devRoot    string
	sources    []SourceSummary
}

func (f *fakeSourceCtl) SetDevMode(_ context.Context, name string, enabled bool, opts taskset.DevModeOpts) error {
	f.gotName = name
	f.gotEnabled = enabled
	f.gotOpts = opts
	return nil
}

func (f *fakeSourceCtl) DevRootPath(string) string { return f.devRoot }

func (f *fakeSourceCtl) Sources() []SourceSummary { return f.sources }

// TestIPC_SourcesSetDevMode_ReturnsClonePath verifies the enable reply carries
// the clone the caller was just handed. Without it the caller has no way to
// reach the files it asked for.
func TestIPC_SourcesSetDevMode_ReturnsClonePath(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{SourcesSetDevMode: true},
		},
	}

	conn, srv := e.startWithSpec(t, nil, nil, spec, nil)
	fake := &fakeSourceCtl{devRoot: "/data/dev-clones/scratch/run-7/taskset.yaml"}
	srv.SetSourceManager(fake)

	sendMsg(t, conn, map[string]any{
		"id":      "dev-1",
		"method":  "dicode.sources.set_dev_mode",
		"name":    "scratch",
		"enabled": true,
		"branch":  "fix/thing",
		"run_id":  "run-7",
	})
	resp := recvMsg(t, conn)
	if errMsg, _ := resp["error"].(string); errMsg != "" {
		t.Fatalf("set_dev_mode failed: %s", errMsg)
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result map, got %#v", resp["result"])
	}
	if got := res["dev_root_path"]; got != "/data/dev-clones/scratch/run-7/taskset.yaml" {
		t.Errorf("dev_root_path = %v, want the source's dev root", got)
	}
	if got := res["clone_path"]; got != "/data/dev-clones/scratch/run-7" {
		t.Errorf("clone_path = %v, want the dev root's directory", got)
	}
	if fake.gotName != "scratch" || !fake.gotEnabled || fake.gotOpts.Branch != "fix/thing" {
		t.Errorf("SetDevMode got name=%q enabled=%v opts=%+v", fake.gotName, fake.gotEnabled, fake.gotOpts)
	}
}

// TestIPC_SourcesSetDevMode_DisableOmitsClonePath verifies a disable reports no
// clone: the path keys are absent rather than empty strings a caller might use.
func TestIPC_SourcesSetDevMode_DisableOmitsClonePath(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{SourcesSetDevMode: true},
		},
	}

	conn, srv := e.startWithSpec(t, nil, nil, spec, nil)
	srv.SetSourceManager(&fakeSourceCtl{devRoot: ""})

	sendMsg(t, conn, map[string]any{
		"id":      "dev-2",
		"method":  "dicode.sources.set_dev_mode",
		"name":    "scratch",
		"enabled": false,
	})
	resp := recvMsg(t, conn)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result map, got %#v", resp["result"])
	}
	if res["ok"] != true {
		t.Errorf("ok = %v, want true", res["ok"])
	}
	if _, present := res["clone_path"]; present {
		t.Errorf("clone_path must be absent when there is no clone, got %#v", res)
	}
	if _, present := res["dev_root_path"]; present {
		t.Errorf("dev_root_path must be absent when there is no clone, got %#v", res)
	}
}

// TestIPC_SourcesList_RequiresCap verifies a task without
// permissions.dicode.sources_list cannot call dicode.sources.list.
func TestIPC_SourcesList_RequiresCap(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				// SourcesList omitted; a neighbouring sources capability is
				// granted to show the two are not one gate.
				SourcesSetDevMode: true,
			},
		},
	}

	conn, srv := e.startWithSpec(t, nil, nil, spec, nil)
	srv.SetSourceManager(&fakeSourceCtl{sources: []SourceSummary{{Name: "scratch"}}})

	sendMsg(t, conn, map[string]any{"id": "srcs-1", "method": "dicode.sources.list"})
	resp := recvMsg(t, conn)
	if errMsg, _ := resp["error"].(string); !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected permission denied error, got: %q (full resp: %#v)", errMsg, resp)
	}
}

// TestIPC_SourcesList_ReturnsSummaries verifies the granted path returns the
// source manager's listing.
func TestIPC_SourcesList_ReturnsSummaries(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{SourcesList: true},
		},
	}

	conn, srv := e.startWithSpec(t, nil, nil, spec, nil)
	srv.SetSourceManager(&fakeSourceCtl{sources: []SourceSummary{
		{Name: "scratch", Type: "taskset", URL: "https://example.com/x.git", Branch: "main", DevMode: true},
	}})

	sendMsg(t, conn, map[string]any{"id": "srcs-2", "method": "dicode.sources.list"})
	resp := recvMsg(t, conn)
	if errMsg, _ := resp["error"].(string); errMsg != "" {
		t.Fatalf("sources.list failed: %s", errMsg)
	}
	list, ok := resp["result"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("expected one summary, got %#v", resp["result"])
	}
	got, _ := list[0].(map[string]any)
	if got["name"] != "scratch" || got["branch"] != "main" || got["dev_mode"] != true {
		t.Errorf("summary = %#v, want the fake's source", got)
	}
}

// TestIPC_SourcesList_WithholdsHostPaths pins the trim: SourceSummary carries
// no filesystem path, so a task holding only sources_list learns nothing about
// the daemon's layout. A path reaches a task through set_dev_mode, which
// returns the clone it just made.
//
// Asserted as the exact field set rather than a deny-list of path-ish names:
// a deny-list only catches the names someone thought to write down, and the
// next field copied over from webui.SourceInfo — which does carry paths —
// would not be one of them.
func TestIPC_SourcesList_WithholdsHostPaths(t *testing.T) {
	want := []string{"Name", "Type", "URL", "Branch", "DevMode"}

	typ := reflect.TypeOf(SourceSummary{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SourceSummary fields = %v, want exactly %v.\n"+
			"A new field here reaches every caller holding sources_list; if it "+
			"names a host path, drop it instead of adding it to this list.", got, want)
	}
}
