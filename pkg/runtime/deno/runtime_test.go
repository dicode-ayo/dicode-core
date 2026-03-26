package deno

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

// testEnv wires up a full Deno runtime for tests.
// Tests are skipped if Deno is not available on the system / download fails.
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
	rt, err := New(reg, secrets.Chain{}, d, zap.NewNop())
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}
	return &testEnv{rt: rt, reg: reg, db: d}
}

func (e *testEnv) run(t *testing.T, script string, opts ...RunOptions) *RunResult {
	t.Helper()
	spec := &task.Spec{
		ID:      "test-task",
		Name:    "test-task",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 30 * time.Second,
	}
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.ts"), []byte(script), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"),
		[]byte("name: test-task\nruntime: deno\ntrigger:\n  manual: true\n"), 0644)
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
	// JSON numbers deserialise to float64.
	if r.ReturnValue != float64(42) {
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
		await log.info("hello")
		await log.warn("world")
		return "done"
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if len(r.Logs) < 2 {
		t.Fatalf("expected ≥2 logs, got %d", len(r.Logs))
	}
	found := map[string]bool{}
	for _, l := range r.Logs {
		if l.Message == "hello" || l.Message == "world" {
			found[l.Message] = true
		}
	}
	if !found["hello"] || !found["world"] {
		t.Errorf("unexpected logs: %+v", r.Logs)
	}
}

func TestRuntime_Env(t *testing.T) {
	e := newTestEnv(t)
	spec := &task.Spec{
		ID:      "env-task",
		Name:    "env-task",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 30 * time.Second,
		Env:     []string{"MY_TOKEN"},
	}
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.ts"), []byte(`return env.get("MY_TOKEN")`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"),
		[]byte("name: env-task\nruntime: deno\ntrigger:\n  manual: true\nenv:\n  - MY_TOKEN\n"), 0644)
	_ = e.reg.Register(spec)

	e.rt.secrets = secrets.Chain{mockSecretProvider{"MY_TOKEN": "tok-123"}}

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
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 30 * time.Second,
		Params:  []task.Param{{Name: "channel", Default: "#general"}},
	}
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.ts"), []byte(`return await params.get("channel")`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"),
		[]byte("name: param-task\nruntime: deno\ntrigger:\n  manual: true\n"), 0644)
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
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer srv.Close()

	e := newTestEnv(t)
	r := e.run(t, `
		const res = await fetch("`+srv.URL+`")
		const body = await res.json()
		return body.ok
	`)
	if r.Error != nil {
		t.Fatalf("error: %v", r.Error)
	}
	if r.ReturnValue != true {
		t.Errorf("expected true, got %v", r.ReturnValue)
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
	if r.ReturnValue != float64(42) {
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
	run, err := e.reg.GetRun(context.Background(), r.RunID)
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
	run, _ := e.reg.GetRun(context.Background(), r.RunID)
	if run.Status != registry.StatusFailure {
		t.Errorf("expected failure, got %s", run.Status)
	}
}

func TestRuntime_Timeout(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	rt, err := New(reg, secrets.Chain{}, d, zap.NewNop())
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}

	spec := &task.Spec{
		ID:      "timeout-task",
		Name:    "timeout-task",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 500 * time.Millisecond,
	}
	dir := t.TempDir()
	spec.TaskDir = dir
	_ = os.WriteFile(filepath.Join(dir, "task.ts"),
		[]byte(`await new Promise(r => setTimeout(r, 30000))`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"),
		[]byte("name: timeout-task\nruntime: deno\ntrigger:\n  manual: true\n"), 0644)
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
