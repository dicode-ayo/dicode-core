package server

import (
	"context"
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

// mockEngine is a test double for EngineRunner.
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

// dial connects to the server's Unix socket.
func dial(t *testing.T, socketPath string) net.Conn {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", socketPath)
		if err == nil {
			return conn
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("dial %s: %v", socketPath, err)
	return nil
}

// send writes a newline-delimited JSON message.
func send(t *testing.T, conn net.Conn, msg interface{}) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", b); err != nil {
		t.Fatal(err)
	}
}

// recv reads one JSON response line.
func recv(t *testing.T, conn net.Conn) map[string]interface{} {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	dec := json.NewDecoder(conn)
	var msg map[string]interface{}
	if err := dec.Decode(&msg); err != nil {
		t.Fatalf("recv: %v", err)
	}
	return msg
}

type testEnv struct {
	srv *Server
	reg *registry.Registry
	db  db.DB
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	return &testEnv{reg: reg, db: d}
}

func (e *testEnv) start(t *testing.T, params map[string]string, input interface{}) (net.Conn, *Server) {
	t.Helper()
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", e.reg, e.db, params, input, zap.NewNop(), nil, nil, "", "", "")
	socketPath, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	return conn, srv
}

func TestServer_Params(t *testing.T) {
	e := newTestEnv(t)
	params := map[string]string{"channel": "#general", "count": "5"}
	conn, _ := e.start(t, params, nil)

	send(t, conn, map[string]interface{}{"id": "1", "method": "params"})
	resp := recv(t, conn)

	if resp["id"] != "1" {
		t.Errorf("wrong id: %v", resp["id"])
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("result is not an object: %v", resp["result"])
	}
	if result["channel"] != "#general" {
		t.Errorf("channel: got %v", result["channel"])
	}
}

func TestServer_Input(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, map[string]interface{}{"msg": "hello"})

	send(t, conn, map[string]interface{}{"id": "1", "method": "input"})
	resp := recv(t, conn)

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("result not an object: %v", resp["result"])
	}
	if result["msg"] != "hello" {
		t.Errorf("msg: got %v", result["msg"])
	}
}

func TestServer_Input_Null(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	send(t, conn, map[string]interface{}{"id": "1", "method": "input"})
	resp := recv(t, conn)

	if resp["result"] != nil {
		t.Errorf("expected null input, got %v", resp["result"])
	}
}

func TestServer_Log(t *testing.T) {
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	// fire-and-forget — no id, no response
	send(t, conn, map[string]interface{}{"method": "log", "level": "info", "message": "test message"})
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

func TestServer_KV_SetGet(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	// kv.set is fire-and-forget
	send(t, conn, map[string]interface{}{"method": "kv.set", "key": "mykey", "value": map[string]interface{}{"n": 42}})
	time.Sleep(20 * time.Millisecond)

	send(t, conn, map[string]interface{}{"id": "1", "method": "kv.get", "key": "mykey"})
	resp := recv(t, conn)

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected object result, got %v (%T)", resp["result"], resp["result"])
	}
	if result["n"] != float64(42) {
		t.Errorf("expected 42, got %v", result["n"])
	}
}

func TestServer_KV_Get_Missing(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	send(t, conn, map[string]interface{}{"id": "1", "method": "kv.get", "key": "nokey"})
	resp := recv(t, conn)

	if resp["result"] != nil {
		t.Errorf("expected null for missing key, got %v", resp["result"])
	}
}

func TestServer_KV_Delete(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	send(t, conn, map[string]interface{}{"method": "kv.set", "key": "delkey", "value": "x"})
	time.Sleep(20 * time.Millisecond)

	// kv.delete is fire-and-forget
	send(t, conn, map[string]interface{}{"method": "kv.delete", "key": "delkey"})
	time.Sleep(20 * time.Millisecond)

	send(t, conn, map[string]interface{}{"id": "1", "method": "kv.get", "key": "delkey"})
	resp := recv(t, conn)

	if resp["result"] != nil {
		t.Errorf("expected null after delete, got %v", resp["result"])
	}
}

func TestServer_KV_Namespacing(t *testing.T) {
	// Two servers sharing the same DB should not see each other's keys.
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)

	makeServer := func(taskID string) (net.Conn, *Server) {
		runID := fmt.Sprintf("run-%s", taskID)
		srv := New(runID, taskID, reg, d, nil, nil, zap.NewNop(), nil, nil, "", "", "")
		sp, err := srv.Start(context.Background())
		if err != nil {
			t.Fatalf("Start %s: %v", taskID, err)
		}
		t.Cleanup(srv.Stop)
		conn := dial(t, sp)
		t.Cleanup(func() { conn.Close() })
		return conn, srv
	}

	connA, _ := makeServer("task-a")
	connB, _ := makeServer("task-b")

	send(t, connA, map[string]interface{}{"method": "kv.set", "key": "shared", "value": "from-a"})
	time.Sleep(20 * time.Millisecond)

	send(t, connB, map[string]interface{}{"id": "1", "method": "kv.get", "key": "shared"})
	resp := recv(t, connB)
	if resp["result"] != nil {
		t.Errorf("task-b should not see task-a's key, got %v", resp["result"])
	}
}

func TestServer_KV_List(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	send(t, conn, map[string]interface{}{"method": "kv.set", "key": "a", "value": 1})
	send(t, conn, map[string]interface{}{"method": "kv.set", "key": "b", "value": 2})
	send(t, conn, map[string]interface{}{"method": "kv.set", "key": "c", "value": 3})
	time.Sleep(30 * time.Millisecond)

	send(t, conn, map[string]interface{}{"id": "1", "method": "kv.list", "prefix": ""})
	resp := recv(t, conn)

	keys, ok := resp["result"].([]interface{})
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

	// fire-and-forget
	send(t, conn, map[string]interface{}{
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

	send(t, conn, map[string]interface{}{"id": "1", "method": "return", "value": "done"})
	resp := recv(t, conn)

	if resp["result"] != true {
		t.Errorf("expected true, got %v", resp["result"])
	}

	// returnCh should carry the value.
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
	// returnCh must be signalled before the reply is sent so that the runtime's
	// select always sees returnCh before doneCh (which only fires after Deno
	// receives the reply and exits).
	e := newTestEnv(t)
	conn, srv := e.start(t, nil, nil)

	send(t, conn, map[string]interface{}{"id": "1", "method": "return", "value": 99})

	// returnCh should be ready without waiting for a read on the conn.
	select {
	case val := <-srv.ReturnCh():
		if val != float64(99) {
			t.Errorf("expected 99, got %v", val)
		}
	case <-time.After(time.Second):
		t.Fatal("returnCh was not signalled before reply read")
	}
}

func TestServer_MalformedRequest_Ignored(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	// Send garbage, then a valid request — server should still work.
	fmt.Fprintf(conn, "not json at all\n") //nolint:errcheck
	time.Sleep(10 * time.Millisecond)

	send(t, conn, map[string]interface{}{"id": "1", "method": "params"})
	resp := recv(t, conn)
	if resp["id"] != "1" {
		t.Errorf("server did not recover after malformed input: %v", resp)
	}
}

// startWithSpec starts a server that has a spec and optional engine wired in.
func startWithSpec(t *testing.T, reg *registry.Registry, database db.DB, spec *task.Spec, eng EngineRunner, aiBaseURL, aiModel, aiAPIKey string) (net.Conn, *Server) {
	t.Helper()
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "test-task", reg, database, nil, nil, zap.NewNop(), spec, eng, aiBaseURL, aiModel, aiAPIKey)
	socketPath, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	return conn, srv
}

func TestServer_Dicode_ListTasks(t *testing.T) {
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "hello-cron", Name: "Hello Cron", Description: "A cron task"})
	_ = e.reg.Register(&task.Spec{ID: "send-report", Name: "Send Report", Description: "Sends a report"})

	conn, _ := e.start(t, nil, nil)

	send(t, conn, map[string]interface{}{"id": "1", "method": "dicode.list_tasks"})
	resp := recv(t, conn)

	tasks, ok := resp["result"].([]interface{})
	if !ok {
		t.Fatalf("expected array result, got %T: %v", resp["result"], resp["result"])
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
	first, ok := tasks[0].(map[string]interface{})
	if !ok {
		t.Fatalf("task entry not an object: %v", tasks[0])
	}
	if first["id"] == nil {
		t.Errorf("task entry missing id field: %v", first)
	}
}

func TestServer_Dicode_GetConfig(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := startWithSpec(t, e.reg, e.db, nil, nil, "https://api.openai.com/v1", "gpt-4o", "sk-test")

	send(t, conn, map[string]interface{}{"id": "1", "method": "dicode.get_config", "section": "ai"})
	resp := recv(t, conn)

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected object result, got %T: %v", resp["result"], resp["result"])
	}
	if result["model"] != "gpt-4o" {
		t.Errorf("model: got %v", result["model"])
	}
	if result["apiKey"] != "sk-test" {
		t.Errorf("apiKey: got %v", result["apiKey"])
	}
	if result["baseURL"] != "https://api.openai.com/v1" {
		t.Errorf("baseURL: got %v", result["baseURL"])
	}
}

func TestServer_Dicode_GetConfig_UnknownSection(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	send(t, conn, map[string]interface{}{"id": "1", "method": "dicode.get_config", "section": "storage"})
	resp := recv(t, conn)

	if resp["error"] == nil {
		t.Errorf("expected error for unknown section, got: %v", resp)
	}
}

func TestServer_Dicode_RunTask_SecurityDenied_NilSpec(t *testing.T) {
	// nil spec → taskAllowed always returns false.
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil) // e.start passes nil spec

	send(t, conn, map[string]interface{}{"id": "1", "method": "dicode.run_task", "taskID": "some-task"})
	resp := recv(t, conn)

	if resp["error"] == nil {
		t.Errorf("expected security error when spec is nil, got: %v", resp)
	}
}

func TestServer_Dicode_RunTask_SecurityDenied_NotAllowed(t *testing.T) {
	e := newTestEnv(t)
	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedTasks: []string{"permitted-task"}},
	}
	conn, _ := startWithSpec(t, e.reg, e.db, spec, nil, "", "", "")

	send(t, conn, map[string]interface{}{"id": "1", "method": "dicode.run_task", "taskID": "forbidden-task"})
	resp := recv(t, conn)

	if resp["error"] == nil {
		t.Errorf("expected security error for unlisted task, got: %v", resp)
	}
}

func TestServer_Dicode_RunTask(t *testing.T) {
	e := newTestEnv(t)
	eng := &mockEngine{
		runID:  "run-abc",
		result: RunResult{RunID: "run-abc", Status: "success"},
	}
	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedTasks: []string{"target-task"}},
	}
	conn, _ := startWithSpec(t, e.reg, e.db, spec, eng, "", "", "")

	send(t, conn, map[string]interface{}{"id": "1", "method": "dicode.run_task", "taskID": "target-task"})
	resp := recv(t, conn)

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected object result, got %T: %v", resp["result"], resp["result"])
	}
	if result["runID"] != "run-abc" {
		t.Errorf("runID: got %v", result["runID"])
	}
	if result["status"] != "success" {
		t.Errorf("status: got %v", result["status"])
	}
}

func TestServer_Dicode_RunTask_Wildcard(t *testing.T) {
	// allowed_tasks: ["*"] permits any task.
	e := newTestEnv(t)
	eng := &mockEngine{runID: "run-1", result: RunResult{RunID: "run-1", Status: "success"}}
	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedTasks: []string{"*"}},
	}
	conn, _ := startWithSpec(t, e.reg, e.db, spec, eng, "", "", "")

	send(t, conn, map[string]interface{}{"id": "1", "method": "dicode.run_task", "taskID": "any-task-name"})
	resp := recv(t, conn)

	if resp["error"] != nil {
		t.Errorf("wildcard should allow any task, got error: %v", resp["error"])
	}
}

func TestServer_MCP_SecurityDenied(t *testing.T) {
	// nil spec → mcpAllowed always returns false.
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	send(t, conn, map[string]interface{}{"id": "1", "method": "mcp.list_tools", "mcpName": "github-mcp"})
	resp := recv(t, conn)

	if resp["error"] == nil {
		t.Errorf("expected security error when spec is nil, got: %v", resp)
	}
}

func TestServer_MCP_ListTools_NoPort(t *testing.T) {
	// Task is allowed but declares no mcp_port — should return an error.
	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "github-mcp"}) // MCPPort = 0

	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedMCP: []string{"github-mcp"}},
	}
	conn, _ := startWithSpec(t, e.reg, e.db, spec, nil, "", "", "")

	send(t, conn, map[string]interface{}{"id": "1", "method": "mcp.list_tools", "mcpName": "github-mcp"})
	resp := recv(t, conn)

	if resp["error"] == nil {
		t.Errorf("expected error when mcp_port is 0, got: %v", resp)
	}
}

func TestServer_MCP_ListTools_Success(t *testing.T) {
	// Start a fake MCP HTTP server that returns one tool.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search repos","inputSchema":{"type":"object","properties":{}}}]}}`) //nolint:errcheck
	}))
	defer ts.Close()

	port := ts.Listener.Addr().(*net.TCPAddr).Port

	e := newTestEnv(t)
	_ = e.reg.Register(&task.Spec{ID: "github-mcp", MCPPort: port})

	spec := &task.Spec{
		ID:       "caller",
		Security: &task.SecurityConfig{AllowedMCP: []string{"github-mcp"}},
	}
	conn, _ := startWithSpec(t, e.reg, e.db, spec, nil, "", "", "")

	send(t, conn, map[string]interface{}{"id": "1", "method": "mcp.list_tools", "mcpName": "github-mcp"})
	resp := recv(t, conn)

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	tools, ok := resp["result"].([]interface{})
	if !ok {
		t.Fatalf("expected array result, got %T: %v", resp["result"], resp["result"])
	}
	if len(tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(tools))
	}
}

func TestServer_MCP_Call_Success(t *testing.T) {
	// Fake MCP server: routes list_tools and tools/call.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req["method"] {
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"found 3 repos"}]}}`) //nolint:errcheck
		default:
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
	conn, _ := startWithSpec(t, e.reg, e.db, spec, nil, "", "", "")

	send(t, conn, map[string]interface{}{
		"id":      "1",
		"method":  "mcp.call",
		"mcpName": "github-mcp",
		"tool":    "search",
		"args":    map[string]interface{}{"query": "dicode"},
	})
	resp := recv(t, conn)

	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if resp["result"] == nil {
		t.Errorf("expected non-nil result")
	}
}
