package ipc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
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
	return e.startWithSpec(t, params, input, nil, nil, "", "", "")
}

func (e *testEnv) startWithSpec(t *testing.T, params map[string]string, input any, spec *task.Spec, eng EngineRunner, aiBaseURL, aiModel, aiAPIKey string) (net.Conn, *Server) {
	t.Helper()
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, params, input, zap.NewNop(), spec, eng, aiBaseURL, aiModel, aiAPIKey)
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
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil, "", "", "")
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
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil, "", "", "")
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

func TestServer_Log(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"method": "log", "level": "info", "message": "test message"})
	time.Sleep(20 * time.Millisecond)

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
		srv := New(runID, taskID, e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil, "", "", "")
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
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil, "", "", "")
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

func TestServer_Dicode_ListTasks(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "hello-cron", Name: "Hello Cron"})
	_ = e.reg.Register(&task.Spec{ID: "send-report", Name: "Send Report"})

	conn, _ := e.start(t, nil, nil)
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

func TestServer_Dicode_GetConfig(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.startWithSpec(t, nil, nil, nil, nil, "https://api.openai.com/v1", "gpt-4o", "sk-test")

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.get_config", "section": "ai"})
	resp := recvMsg(t, conn)

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", resp["result"])
	}
	if result["model"] != "gpt-4o" {
		t.Errorf("model: %v", result["model"])
	}
	if result["baseURL"] != "https://api.openai.com/v1" {
		t.Errorf("baseURL: %v", result["baseURL"])
	}
	// apiKey must never be returned to task scripts (security: credential exposure).
	if _, present := result["apiKey"]; present {
		t.Errorf("apiKey must not be returned by get_config: %v", result["apiKey"])
	}
}

func TestServer_Dicode_GetConfig_UnknownSection(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.get_config", "section": "storage"})
	resp := recvMsg(t, conn)

	if resp["error"] == nil {
		t.Errorf("expected error for unknown section")
	}
}

func TestServer_Dicode_RunTask_SecurityDenied_NilSpec(t *testing.T) {
	e := newTestEnv(t)
	// Must issue token with tasks.trigger cap but nil spec → taskAllowed=false.
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil, "", "", "")
	socketPath, _, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	caps := append(defaultTaskCaps(), CapTaskTrigger)
	tok, _ := IssueToken(e.secret, "task:test-task", runID, caps)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, tok)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "some-task"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected security error when spec is nil")
	}
}

func TestServer_Dicode_RunTask_SecurityDenied_NotAllowed(t *testing.T) {
	e := newTestEnv(t)
	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedTasks: []string{"permitted-task"}},
	}
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil, "", "", "")

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "forbidden-task"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected security error for unlisted task")
	}
}

func TestServer_Dicode_RunTask(t *testing.T) {
	e := newTestEnv(t)
	eng := &mockEngine{runID: "run-abc", result: RunResult{RunID: "run-abc", Status: "success"}}
	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedTasks: []string{"target-task"}},
	}
	conn, _ := e.startWithSpec(t, nil, nil, spec, eng, "", "", "")

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
	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedTasks: []string{"*"}},
	}
	conn, _ := e.startWithSpec(t, nil, nil, spec, eng, "", "", "")

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "any-task"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Errorf("wildcard should allow any task, got: %v", resp["error"])
	}
}

// ── mcp.* tests ───────────────────────────────────────────────────────────────

func TestServer_MCP_SecurityDenied(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil) // default caps — no mcp.call

	sendMsg(t, conn, map[string]any{"id": "1", "method": "mcp.list_tools", "mcpName": "github-mcp"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected permission denied for mcp.list_tools without mcp.call cap")
	}
}

func TestServer_MCP_ListTools_NoPort(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "github-mcp"}) // MCPPort = 0

	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedMCP: []string{"github-mcp"}},
	}
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil, "", "", "")

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
	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedMCP: []string{"github-mcp"}},
	}
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil, "", "", "")

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
	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedMCP: []string{"github-mcp"}},
	}
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil, "", "", "")

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
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil, "", "", "")
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
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil, "", "", "")
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
