package trigger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	jsruntime "github.com/dicode/dicode/pkg/runtime/js"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

type testEnv struct {
	engine *Engine
	reg    *registry.Registry
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	rt := jsruntime.New(reg, secrets.Chain{}, d, zap.NewNop())
	eng := New(reg, rt, zap.NewNop())
	return &testEnv{engine: eng, reg: reg}
}

func writeTask(t *testing.T, dir, id, script string, trigger task.TriggerConfig) *task.Spec {
	t.Helper()
	td := filepath.Join(dir, id)
	_ = os.MkdirAll(td, 0755)
	yaml := "name: " + id + "\ntrigger:\n  manual: true\nruntime: js\n"
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0644)
	_ = os.WriteFile(filepath.Join(td, "task.js"), []byte(script), 0644)
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeJS,
		Trigger: trigger,
		Timeout: 5 * time.Second,
		TaskDir: td,
	}
	return spec
}

func TestEngine_FireManual(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "manual-task", `return "manual-ok"`, task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "manual-task", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	if runID == "" {
		t.Fatal("empty run ID")
	}

	// FireManual is async; poll until the run finishes (up to 5s).
	var run *registry.Run
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err = e.reg.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != registry.StatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if run.Status != registry.StatusSuccess {
		t.Errorf("expected success, got %s", run.Status)
	}
}

func TestEngine_FireManual_NotFound(t *testing.T) {
	e := newTestEnv(t)
	_, err := e.engine.FireManual(context.Background(), "missing-task", nil)
	if err == nil {
		t.Fatal("expected error for missing task")
	}
}

func TestEngine_WebhookHandler(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "hook-task", `return "webhook-ok"`, task.TriggerConfig{Webhook: "/hooks/my-hook"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	handler := e.engine.WebhookHandler()

	req := httptest.NewRequest(http.MethodPost, "/hooks/my-hook", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "runId") {
		t.Errorf("expected runId in response, got: %s", w.Body.String())
	}
}

func TestEngine_WebhookHandler_NotFound(t *testing.T) {
	e := newTestEnv(t)
	handler := e.engine.WebhookHandler()

	req := httptest.NewRequest(http.MethodPost, "/hooks/missing", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestEngine_Chain(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// task-a returns a value; task-b listens for task-a completion.
	specA := writeTask(t, dir, "task-a", `return { msg: "from-a" }`, task.TriggerConfig{Manual: true})
	specB := writeTask(t, dir, "task-b", `return input.msg`, task.TriggerConfig{
		Chain: &task.ChainTrigger{From: "task-a", On: "success"},
	})

	_ = e.reg.Register(specA)
	_ = e.reg.Register(specB)

	runID, err := e.engine.FireManual(context.Background(), "task-a", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	if runID == "" {
		t.Fatal("no run ID")
	}

	// Give chain goroutine time to complete.
	time.Sleep(300 * time.Millisecond)

	// task-b should have a run record now.
	runs, err := e.reg.ListRuns(context.Background(), "task-b", 5)
	if err != nil {
		t.Fatalf("ListRuns task-b: %v", err)
	}
	if len(runs) == 0 {
		t.Fatal("task-b was not triggered by chain")
	}
	if runs[0].Status != registry.StatusSuccess {
		t.Errorf("task-b run status: %s", runs[0].Status)
	}
}

func TestEngine_Chain_OnFailure(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	specA := writeTask(t, dir, "fail-a", `throw new Error("boom")`, task.TriggerConfig{Manual: true})
	specB := writeTask(t, dir, "on-fail-b", `return "handled"`, task.TriggerConfig{
		Chain: &task.ChainTrigger{From: "fail-a", On: "failure"},
	})

	_ = e.reg.Register(specA)
	_ = e.reg.Register(specB)

	e.engine.FireManual(context.Background(), "fail-a", nil)
	time.Sleep(300 * time.Millisecond)

	runs, _ := e.reg.ListRuns(context.Background(), "on-fail-b", 5)
	if len(runs) == 0 {
		t.Fatal("on-fail-b not triggered")
	}
}

func TestEngine_Register_Unregister(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "wh-task", `return 1`, task.TriggerConfig{Webhook: "/hooks/test"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	// Webhook should be registered.
	e.engine.mu.Lock()
	_, ok := e.engine.webhooks["/hooks/test"]
	e.engine.mu.Unlock()
	if !ok {
		t.Fatal("webhook not registered")
	}

	e.engine.Unregister("wh-task")

	e.engine.mu.Lock()
	_, ok = e.engine.webhooks["/hooks/test"]
	e.engine.mu.Unlock()
	if ok {
		t.Fatal("webhook should be unregistered")
	}
}

func TestEngine_Cron_Register(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "cron-task", `return 1`, task.TriggerConfig{Cron: "* * * * *"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	e.engine.mu.Lock()
	_, ok := e.engine.cronEntries["cron-task"]
	e.engine.mu.Unlock()
	if !ok {
		t.Fatal("cron entry not registered")
	}

	e.engine.Unregister("cron-task")

	e.engine.mu.Lock()
	_, ok = e.engine.cronEntries["cron-task"]
	e.engine.mu.Unlock()
	if ok {
		t.Fatal("cron entry should be removed")
	}
}
