package js

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// testEnv wires up a full runtime for tests.
type testEnv struct {
	rt  *Runtime
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
	chain := secrets.Chain{} // no secrets in tests
	rt := New(reg, chain, d, zap.NewNop())
	return &testEnv{rt: rt, reg: reg, db: d}
}

func (e *testEnv) run(t *testing.T, script string, opts ...RunOptions) *RunResult {
	t.Helper()
	spec := &task.Spec{
		ID:      "test-task",
		Name:    "test-task",
		Runtime: task.RuntimeJS,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
	}
	// Write script to a temp dir so spec.Script() can read it.
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.js"), []byte(script), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: test-task\ntrigger:\n  manual: true\n"), 0644)
	_ = e.reg.Register(spec)

	o := RunOptions{}
	if len(opts) > 0 {
		o = opts[0]
	}
	result, err := e.rt.Run(context.Background(), spec, o)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	return result
}

// --- tests ---

func TestRuntime_ReturnValue(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return 42`)
	if r.Error != nil {
		t.Fatalf("unexpected error: %v", r.Error)
	}
	if r.ReturnValue != int64(42) {
		t.Errorf("expected 42, got %v (%T)", r.ReturnValue, r.ReturnValue)
	}
}

func TestRuntime_ReturnString(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return "hello"`)
	if r.ReturnValue != "hello" {
		t.Errorf("got %v", r.ReturnValue)
	}
}

func TestRuntime_AsyncAwait(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `
		const p = new Promise(resolve => setTimeout(resolve, 10, "async-result"))
		const v = await p
		return v
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != "async-result" {
		t.Errorf("got %v", r.ReturnValue)
	}
}

func TestRuntime_Log(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `
		log.info("hello")
		log.warn("world")
		return "done"
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if len(r.Logs) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(r.Logs))
	}
	if r.Logs[0].Message != "hello" || r.Logs[1].Level != "warn" {
		t.Errorf("unexpected logs: %+v", r.Logs)
	}
}

func TestRuntime_Env(t *testing.T) {
	e := newTestEnv(t)
	spec := &task.Spec{
		ID:      "env-task",
		Name:    "env-task",
		Runtime: task.RuntimeJS,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
		Env:     []string{"MY_TOKEN"},
	}
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.js"), []byte(`return env.get("MY_TOKEN")`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: env-task\ntrigger:\n  manual: true\nenv:\n  - MY_TOKEN\n"), 0644)
	_ = e.reg.Register(spec)

	// Wire a secret.
	mockProvider := &mockSecretProvider{"MY_TOKEN": "tok-123"}
	e.rt.secrets = secrets.Chain{mockProvider}

	result, _ := e.rt.Run(context.Background(), spec, RunOptions{})
	if result.ReturnValue != "tok-123" {
		t.Errorf("expected tok-123, got %v", result.ReturnValue)
	}
}

func TestRuntime_Params(t *testing.T) {
	e := newTestEnv(t)
	spec := &task.Spec{
		ID:      "param-task",
		Name:    "param-task",
		Runtime: task.RuntimeJS,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
		Params:  []task.Param{{Name: "channel", Default: "#general"}},
	}
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.js"), []byte(`return params.get("channel")`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: param-task\ntrigger:\n  manual: true\n"), 0644)
	_ = e.reg.Register(spec)

	result, _ := e.rt.Run(context.Background(), spec, RunOptions{
		Params: map[string]string{"channel": "#devops"},
	})
	if result.ReturnValue != "#devops" {
		t.Errorf("expected #devops, got %v", result.ReturnValue)
	}
}

func TestRuntime_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	e := newTestEnv(t)
	r := e.run(t, `
		const res = await http.get("`+srv.URL+`")
		return res.body.ok
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != true {
		t.Errorf("expected true, got %v", r.ReturnValue)
	}
}

func TestRuntime_HTTP_Interceptor(t *testing.T) {
	e := newTestEnv(t)
	intercepted := false
	r := e.run(t, `
		const res = await http.post("https://example.com/api", { body: { x: 1 } })
		return res.status
	`, RunOptions{
		HTTPInterceptor: func(method, url string, body []byte) (int, []byte, bool) {
			intercepted = true
			return 201, []byte(`{"created":true}`), true
		},
	})
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if !intercepted {
		t.Error("interceptor not called")
	}
	if r.ReturnValue != int64(201) {
		t.Errorf("expected 201, got %v", r.ReturnValue)
	}
}

func TestRuntime_KV(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `
		await kv.set("mykey", { count: 42 })
		const val = await kv.get("mykey")
		return val.count
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	// JSON number round-trips as int64 via goja export.
	if r.ReturnValue != int64(42) && r.ReturnValue != float64(42) {
		t.Errorf("expected 42, got %v (%T)", r.ReturnValue, r.ReturnValue)
	}
}

func TestRuntime_Output_HTML(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return output.html("<h1>Hello</h1>")`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.Output == nil {
		t.Fatal("expected output to be set")
	}
	if r.Output.ContentType != "text/html" {
		t.Errorf("expected text/html, got %s", r.Output.ContentType)
	}
	if r.Output.Content != "<h1>Hello</h1>" {
		t.Errorf("unexpected content: %s", r.Output.Content)
	}
}

func TestRuntime_Output_Text(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return output.text("plain text result")`)
	if r.Output == nil || r.Output.ContentType != "text/plain" {
		t.Fatalf("expected text/plain output, got %+v", r.Output)
	}
}

func TestRuntime_RunRecord(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `return "ok"`)
	if r.RunID == "" {
		t.Fatal("no run ID")
	}
	ctx := context.Background()
	run, err := e.reg.GetRun(ctx, r.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != registry.StatusSuccess {
		t.Errorf("expected success, got %s", run.Status)
	}
}

func TestRuntime_ScriptError(t *testing.T) {
	e := newTestEnv(t)
	r := e.run(t, `throw new Error("boom")`)
	if r.Error == nil {
		t.Fatal("expected error")
	}
	// Run should be marked failed in DB.
	run, _ := e.reg.GetRun(context.Background(), r.RunID)
	if run.Status != registry.StatusFailure {
		t.Errorf("expected failure, got %s", run.Status)
	}
}

func TestRuntime_Timeout(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	rt := New(reg, secrets.Chain{}, d, zap.NewNop())

	spec := &task.Spec{
		ID:      "timeout-task",
		Name:    "timeout-task",
		Runtime: task.RuntimeJS,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 200 * time.Millisecond,
	}
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.js"), []byte(`await new Promise(r => setTimeout(r, 5000))`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: timeout-task\ntrigger:\n  manual: true\n"), 0644)
	_ = reg.Register(spec)

	r, _ := rt.Run(context.Background(), spec, RunOptions{})
	if r.Error == nil {
		t.Fatal("expected timeout error")
	}
}

// mockSecretProvider for env tests.
type mockSecretProvider map[string]string

func (m mockSecretProvider) Name() string { return "mock" }
func (m mockSecretProvider) Get(_ context.Context, key string) (string, error) {
	return m[key], nil
}
