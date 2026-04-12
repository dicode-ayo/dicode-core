package trigger

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
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
	rt, err := denoruntime.New(reg, secrets.Chain{}, d, zap.NewNop())
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}
	eng := New(reg, rt, zap.NewNop())
	return &testEnv{engine: eng, reg: reg}
}

func writeTask(t *testing.T, dir, id, script string, trigger task.TriggerConfig) *task.Spec {
	t.Helper()
	td := filepath.Join(dir, id)
	_ = os.MkdirAll(td, 0755)
	yaml := "name: " + id + "\ntrigger:\n  manual: true\nruntime: deno\n"
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0644)
	_ = os.WriteFile(filepath.Join(td, "task.ts"), []byte(script), 0644)
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: trigger,
		Timeout: 30 * time.Second,
		TaskDir: td,
	}
	return spec
}

func TestEngine_FireManual(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "manual-task", `export default async function main() { return "manual-ok" }`, task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "manual-task", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	if runID == "" {
		t.Fatal("empty run ID")
	}

	// FireManual is async; poll until the run finishes (up to 30s for deno startup).
	var run *registry.Run
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		run, err = e.reg.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != registry.StatusRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
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
	spec := writeTask(t, dir, "hook-task", `export default async function main() { return "webhook-ok" }`, task.TriggerConfig{Webhook: "/hooks/my-hook"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	handler := e.engine.WebhookHandler()

	req := httptest.NewRequest(http.MethodPost, "/hooks/my-hook", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Sync mode: response is the task's return value with X-Run-Id header.
	if !strings.Contains(w.Body.String(), "webhook-ok") {
		t.Errorf("expected return value in response, got: %s", w.Body.String())
	}
	if w.Header().Get("X-Run-Id") == "" {
		t.Errorf("expected X-Run-Id header")
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

	specA := writeTask(t, dir, "task-a", `export default async function main() { return { msg: "from-a" } }`, task.TriggerConfig{Manual: true})
	specB := writeTask(t, dir, "task-b", `export default async function main({ input }) { return input.msg }`, task.TriggerConfig{
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

	// Wait for both task-a and the chained task-b to complete.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := e.reg.ListRuns(context.Background(), "task-b", 5)
		if len(runs) > 0 && runs[0].Status != registry.StatusRunning {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

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

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := e.reg.ListRuns(context.Background(), "on-fail-b", 5)
		if len(runs) > 0 && runs[0].Status != registry.StatusRunning {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

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

// newMinimalEngine creates an Engine without a real executor, suitable for
// tests that only exercise HTTP serving (UI pages, assets) without task execution.
func newMinimalEngine(t *testing.T) (*Engine, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := New(reg, nil, zap.NewNop())
	return eng, reg
}

// writeUITask creates a task directory with a task.yaml, optional task.ts,
// and optional extra files (filename → content).
func writeUITask(t *testing.T, dir, id string, trigger task.TriggerConfig, extraFiles map[string]string) *task.Spec {
	t.Helper()
	td := filepath.Join(dir, id)
	_ = os.MkdirAll(td, 0755)
	yaml := "name: " + id + "\nruntime: deno\n"
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0644)
	for name, content := range extraFiles {
		_ = os.WriteFile(filepath.Join(td, name), []byte(content), 0644)
	}
	return &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: trigger,
		TaskDir: td,
	}
}

func TestEngine_WebhookHandler_ServesIndexHTML(t *testing.T) {
	dir := t.TempDir()
	eng, reg := newMinimalEngine(t)

	const indexContent = `<html><head><title>My UI</title></head><body>Hello</body></html>`
	spec := writeUITask(t, dir, "ui-task", task.TriggerConfig{Webhook: "/hooks/ui-task"}, map[string]string{
		"index.html": indexContent,
	})
	_ = reg.Register(spec)
	eng.Register(spec)

	handler := eng.WebhookHandler()
	req := httptest.NewRequest(http.MethodGet, "/hooks/ui-task", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("expected text/html content type, got %s", w.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, "/dicode.js") {
		t.Errorf("expected dicode.js injection, got: %s", body)
	}
	if !strings.Contains(body, `content="ui-task"`) {
		t.Errorf("expected dicode-task meta tag, got: %s", body)
	}
	if !strings.Contains(body, "Hello") {
		t.Errorf("expected original HTML content, got: %s", body)
	}
}

func TestEngine_WebhookHandler_NoIndexHTML_RunsTask(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "plain-hook", `export default async function main() { return "ran" }`, task.TriggerConfig{Webhook: "/hooks/plain-hook"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	handler := e.engine.WebhookHandler()
	req := httptest.NewRequest(http.MethodGet, "/hooks/plain-hook", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ran") {
		t.Errorf("expected task output, got: %s", w.Body.String())
	}
}

func TestEngine_WebhookHandler_ServesAsset(t *testing.T) {
	dir := t.TempDir()
	eng, reg := newMinimalEngine(t)

	const cssContent = `body { color: red; }`
	spec := writeUITask(t, dir, "asset-task", task.TriggerConfig{Webhook: "/hooks/asset-task"}, map[string]string{
		"index.html": `<html><head></head><body></body></html>`,
		"style.css":  cssContent,
	})
	_ = reg.Register(spec)
	eng.Register(spec)

	handler := eng.WebhookHandler()
	req := httptest.NewRequest(http.MethodGet, "/hooks/asset-task/style.css", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/css") {
		t.Errorf("expected text/css, got %s", w.Header().Get("Content-Type"))
	}
	if w.Body.String() != cssContent {
		t.Errorf("expected CSS content, got: %s", w.Body.String())
	}
}

func TestEngine_WebhookHandler_AssetTraversalBlocked(t *testing.T) {
	dir := t.TempDir()
	eng, reg := newMinimalEngine(t)

	spec := writeUITask(t, dir, "traversal-task", task.TriggerConfig{Webhook: "/hooks/traversal-task"}, map[string]string{
		"index.html": `<html><head></head><body></body></html>`,
	})
	_ = reg.Register(spec)
	eng.Register(spec)

	handler := eng.WebhookHandler()

	for _, dangerous := range []string{
		"/hooks/traversal-task/../../../etc/passwd",
		"/hooks/traversal-task/%2e%2e/secret",
	} {
		req := httptest.NewRequest(http.MethodGet, dangerous, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("path traversal not blocked for %q: got 200", dangerous)
		}
	}
}

func TestEngine_WebhookHandler_AssetUnknownTypeBlocked(t *testing.T) {
	dir := t.TempDir()
	eng, reg := newMinimalEngine(t)

	spec := writeUITask(t, dir, "blocked-task", task.TriggerConfig{Webhook: "/hooks/blocked-task"}, map[string]string{
		"index.html": `<html><head></head><body></body></html>`,
		"task.ts":    `return 1`,
	})
	_ = reg.Register(spec)
	eng.Register(spec)

	handler := eng.WebhookHandler()
	req := httptest.NewRequest(http.MethodGet, "/hooks/blocked-task/task.ts", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for .ts file, got %d", w.Code)
	}
}

func TestEngine_WebhookHandler_FormPOST(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "form-task", `return params.get("name")`, task.TriggerConfig{Webhook: "/hooks/form-task"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	handler := e.engine.WebhookHandler()

	req := httptest.NewRequest(http.MethodPost, "/hooks/form-task",
		strings.NewReader("name=Alice"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Form POST should redirect to /runs/{id}/result.
	if w.Code != http.StatusSeeOther {
		t.Errorf("expected 303 redirect, got %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/runs/") {
		t.Errorf("expected redirect to /runs/..., got %q", loc)
	}
}

// TestCronCatchup verifies that cron tasks whose next_run_at is in the past
// are fired with trigger_source="cron-catchup".
// Calls catchupMissedCronRuns directly (same package) to avoid the goroutine
// race that would arise from calling Start() and then polling.
func TestCronCatchup(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)

	dir := t.TempDir()
	spec := writeTask(t, dir, "catchup-task", `return 1`, task.TriggerConfig{Cron: "0 9 * * *"})
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	// Seed a stale cron_jobs row simulating the previous session.
	missedAt := time.Now().Add(-5 * time.Minute).Unix()
	if err := d.Exec(context.Background(),
		`INSERT INTO cron_jobs(task_id,cron_expr,next_run_at) VALUES(?,?,?)`,
		"catchup-task", "0 9 * * *", missedAt,
	); err != nil {
		t.Fatalf("seed cron_jobs: %v", err)
	}

	eng := New(reg, nil, zap.NewNop()) // nil executor — dispatch fails but run record is created
	eng.SetDB(d)

	// Call directly — synchronous, no goroutine scheduling race.
	// fireAsync creates the run record before launching its goroutine, so
	// ListRuns is reliable immediately after this returns.
	eng.catchupMissedCronRuns(context.Background())

	runs, err := reg.ListRuns(context.Background(), "catchup-task", 5)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var found bool
	for _, r := range runs {
		if r.TriggerSource == "cron-catchup" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a cron-catchup run to be created")
	}
}

// TestCronCatchup_OrphanRow verifies that a task deleted between sessions
// produces a warning log and does not panic.
func TestCronCatchup_OrphanRow(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)

	// Seed a cron_jobs row for a task that is NOT in the registry.
	missedAt := time.Now().Add(-5 * time.Minute).Unix()
	if err := d.Exec(context.Background(),
		`INSERT INTO cron_jobs(task_id,cron_expr,next_run_at) VALUES(?,?,?)`,
		"deleted-task", "* * * * *", missedAt,
	); err != nil {
		t.Fatalf("seed cron_jobs: %v", err)
	}

	eng := New(reg, nil, zap.NewNop())
	eng.SetDB(d)

	// Should not panic; orphan row is skipped with a warning.
	eng.catchupMissedCronRuns(context.Background())

	runs, _ := reg.ListRuns(context.Background(), "deleted-task", 5)
	if len(runs) != 0 {
		t.Errorf("expected no runs for deleted task, got %d", len(runs))
	}
}

// TestCronCatchup_TooOld verifies that a missed run older than 24h is skipped
// (not fired) and produces a Warn log, not a catchup run.
func TestCronCatchup_TooOld(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)

	dir := t.TempDir()
	spec := writeTask(t, dir, "old-cron", `return 1`, task.TriggerConfig{Cron: "* * * * *"})
	_ = reg.Register(spec)

	// Seed a next_run_at more than 24h in the past.
	tooOldAt := time.Now().Add(-25 * time.Hour).Unix()
	if err := d.Exec(context.Background(),
		`INSERT INTO cron_jobs(task_id,cron_expr,next_run_at) VALUES(?,?,?)`,
		"old-cron", "* * * * *", tooOldAt,
	); err != nil {
		t.Fatalf("seed cron_jobs: %v", err)
	}

	eng := New(reg, nil, zap.NewNop())
	eng.SetDB(d)
	eng.catchupMissedCronRuns(context.Background())

	// No run should be created — the row is too old.
	runs, _ := reg.ListRuns(context.Background(), "old-cron", 5)
	for _, r := range runs {
		if r.TriggerSource == "cron-catchup" {
			t.Error("expected no cron-catchup run for a >24h old missed run")
		}
	}
}

// TestCronPersistence verifies that registering a cron task writes a cron_jobs
// row and unregistering it removes the row.
func TestCronPersistence(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := New(reg, nil, zap.NewNop())
	eng.SetDB(d)

	dir := t.TempDir()
	spec := writeTask(t, dir, "persist-cron", `return 1`, task.TriggerConfig{Cron: "* * * * *"})
	_ = reg.Register(spec)
	eng.Register(spec)

	// Row must exist after Register.
	var count int
	_ = d.Query(context.Background(),
		`SELECT COUNT(*) FROM cron_jobs WHERE task_id=?`, []any{"persist-cron"},
		func(rows db.Scanner) error {
			rows.Next()
			return rows.Scan(&count)
		},
	)
	if count != 1 {
		t.Errorf("expected 1 cron_jobs row after Register, got %d", count)
	}

	eng.Unregister("persist-cron")

	// Row must be gone after Unregister.
	count = 0
	_ = d.Query(context.Background(),
		`SELECT COUNT(*) FROM cron_jobs WHERE task_id=?`, []any{"persist-cron"},
		func(rows db.Scanner) error {
			rows.Next()
			return rows.Scan(&count)
		},
	)
	if count != 0 {
		t.Errorf("expected 0 cron_jobs rows after Unregister, got %d", count)
	}
}

func TestInjectDicodeSDK(t *testing.T) {
	html := `<html><head><title>Test</title></head><body></body></html>`
	dummyReq, _ := http.NewRequest(http.MethodGet, "http://localhost/hooks/my-task", nil)
	result := injectDicodeSDK(html, "/hooks/my-task", "my-task", dummyReq)

	if !strings.Contains(result, `<base href="/hooks/my-task/">`) {
		t.Error("base tag not injected")
	}
	if !strings.Contains(result, `<script src="/dicode.js"></script>`) {
		t.Error("dicode.js script tag not injected")
	}
	if !strings.Contains(result, `content="my-task"`) {
		t.Error("dicode-task meta not injected")
	}
	if !strings.Contains(result, `content="/hooks/my-task"`) {
		t.Error("dicode-hook meta not injected")
	}
	// <base> must appear before any other head element (i.e. right after <head>).
	baseIdx := strings.Index(result, "<base ")
	linkIdx := strings.Index(result, "<title>")
	if baseIdx > linkIdx {
		t.Error("<base> tag must appear before other <head> elements")
	}
}

func TestInjectDicodeSDK_WithRelayBase(t *testing.T) {
	input := `<html><head><title>Test</title></head><body></body></html>`
	r := httptest.NewRequest(http.MethodGet, "/hooks/my-task", nil)
	r.Header.Set("X-Relay-Base", "/u/"+strings.Repeat("ab", 32))

	result := injectDicodeSDK(input, "/hooks/my-task", "my-task", r)

	expectedBase := "/u/" + strings.Repeat("ab", 32)
	if !strings.Contains(result, `<base href="`+expectedBase+`/hooks/my-task/">`) {
		t.Errorf("expected relay-prefixed base href, got: %s", result)
	}
	if !strings.Contains(result, `content="`+expectedBase+`/hooks/my-task"`) {
		t.Errorf("expected relay-prefixed dicode-hook meta, got: %s", result)
	}
	if !strings.Contains(result, `src="`+expectedBase+`/dicode.js"`) {
		t.Errorf("expected relay-prefixed dicode.js src, got: %s", result)
	}
}

func TestInjectDicodeSDK_InvalidRelayBase(t *testing.T) {
	input := `<html><head></head><body></body></html>`
	r := httptest.NewRequest(http.MethodGet, "/hooks/my-task", nil)
	r.Header.Set("X-Relay-Base", `"><script>alert(1)</script><x a="`)

	result := injectDicodeSDK(input, "/hooks/my-task", "my-task", r)

	// Invalid relay base should be ignored — output should NOT contain the injection
	if strings.Contains(result, "alert(1)") {
		t.Error("XSS injection was not blocked")
	}
	// Should fall back to local paths
	if !strings.Contains(result, `<base href="/hooks/my-task/">`) {
		t.Errorf("expected local base href fallback, got: %s", result)
	}
}

func TestInjectDicodeSDK_NoHead(t *testing.T) {
	html := `<body>No head tag</body>`
	dummyReq, _ := http.NewRequest(http.MethodGet, "http://localhost/hooks/x", nil)
	result := injectDicodeSDK(html, "/hooks/x", "x", dummyReq)
	if !strings.Contains(result, "dicode.js") {
		t.Error("dicode.js not injected when no </head> present")
	}
}

// TestSetMaxConcurrentTasks_Unlimited verifies that the default (0) semaphore
// setting leaves taskSem nil, preserving backwards-compatible unlimited behaviour.
func TestSetMaxConcurrentTasks_Unlimited(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	reg := registry.New(d)
	eng := New(reg, nil, zap.NewNop())

	// Default: no semaphore.
	if eng.taskSem != nil {
		t.Fatal("expected taskSem to be nil by default (unlimited)")
	}

	// Explicitly set to 0 → still nil.
	eng.SetMaxConcurrentTasks(0)
	if eng.taskSem != nil {
		t.Fatal("expected taskSem to be nil after SetMaxConcurrentTasks(0)")
	}
}

// TestSetMaxConcurrentTasks_SetAndReset verifies that SetMaxConcurrentTasks
// correctly sets and clears the semaphore channel.
func TestSetMaxConcurrentTasks_SetAndReset(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	reg := registry.New(d)
	eng := New(reg, nil, zap.NewNop())

	eng.SetMaxConcurrentTasks(3)
	if eng.taskSem == nil {
		t.Fatal("expected taskSem to be non-nil after SetMaxConcurrentTasks(3)")
	}
	if cap(eng.taskSem) != 3 {
		t.Fatalf("expected semaphore capacity 3, got %d", cap(eng.taskSem))
	}

	// Resetting to 0 clears the semaphore.
	eng.SetMaxConcurrentTasks(0)
	if eng.taskSem != nil {
		t.Fatal("expected taskSem to be nil after SetMaxConcurrentTasks(0)")
	}
}

// TestConcurrencyGetters verifies the public accessors used by /api/metrics
// report consistent values for unlimited, configured-but-idle, and reset states.
func TestConcurrencyGetters(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	reg := registry.New(d)
	eng := New(reg, nil, zap.NewNop())

	// Unlimited defaults.
	if got := eng.MaxConcurrentTasks(); got != 0 {
		t.Errorf("MaxConcurrentTasks() default = %d, want 0", got)
	}
	if got := eng.ActiveTaskSlots(); got != 0 {
		t.Errorf("ActiveTaskSlots() default = %d, want 0", got)
	}
	if got := eng.WaitingTasks(); got != 0 {
		t.Errorf("WaitingTasks() default = %d, want 0", got)
	}

	// Configured but idle.
	eng.SetMaxConcurrentTasks(5)
	if got := eng.MaxConcurrentTasks(); got != 5 {
		t.Errorf("MaxConcurrentTasks() = %d, want 5", got)
	}
	if got := eng.ActiveTaskSlots(); got != 0 {
		t.Errorf("ActiveTaskSlots() idle = %d, want 0", got)
	}

	// Reset clears cap.
	eng.SetMaxConcurrentTasks(0)
	if got := eng.MaxConcurrentTasks(); got != 0 {
		t.Errorf("MaxConcurrentTasks() after reset = %d, want 0", got)
	}
}

// TestFireAsync_ConcurrencyLimit verifies that with MaxConcurrentTasks=2 at
// most 2 task goroutines execute simultaneously.
func TestFireAsync_ConcurrencyLimit(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	reg := registry.New(d)

	const limit = 2
	const total = 5

	// inflight tracks how many tasks are running concurrently.
	var inflight atomic.Int32
	var maxSeen atomic.Int32

	// blockCh gates each task: send a value to unblock one goroutine.
	blockCh := make(chan struct{}, total)

	// atLimitCh is closed when inflight first reaches the concurrency limit,
	// giving the test a reliable signal without sleeping.
	atLimitCh := make(chan struct{})
	var atLimitOnce sync.Once

	// Build a fake executor that records concurrency.
	fakeExec := &fakeExecutor{
		fn: func() {
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			// Record the peak.
			for {
				old := maxSeen.Load()
				if cur <= old || maxSeen.CompareAndSwap(old, cur) {
					break
				}
			}
			// Signal the test once we have reached the semaphore limit.
			if cur >= int32(limit) {
				atLimitOnce.Do(func() { close(atLimitCh) })
			}
			// Block until the test unblocks us.
			<-blockCh
		},
	}

	eng := New(reg, fakeExec, zap.NewNop())
	eng.SetMaxConcurrentTasks(limit)

	// Provide a shutdown context so fireAsync can select on it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.shutdownMu.Lock()
	eng.shutdownCtx = ctx
	eng.shutdownMu.Unlock()

	dir := t.TempDir()
	var specs []*task.Spec
	for i := range total {
		id := fmt.Sprintf("sem-task-%d", i)
		specs = append(specs, writeTask(t, dir, id, `return 1`, task.TriggerConfig{Manual: true}))
		if err := reg.Register(specs[i]); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	// Fire all tasks asynchronously.
	for i := range total {
		if _, err := eng.fireAsync(ctx, specs[i], pkgruntime.RunOptions{}, "test"); err != nil {
			t.Fatalf("fireAsync %d: %v", i, err)
		}
	}

	// Wait until the semaphore is full before unblocking tasks.
	select {
	case <-atLimitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inflight tasks to reach concurrency limit")
	}

	// Unblock all tasks.
	for range total {
		blockCh <- struct{}{}
	}

	// Wait for all goroutines to drain.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if inflight.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if maxSeen.Load() > int32(limit) {
		t.Errorf("concurrency limit exceeded: saw %d concurrent tasks (limit %d)", maxSeen.Load(), limit)
	}
}

// TestFireAsync_ShutdownUnblocksWaiting verifies that cancelling the shutdown
// context releases goroutines that are blocked waiting for a semaphore slot.
func TestFireAsync_ShutdownUnblocksWaiting(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	reg := registry.New(d)

	holdCh := make(chan struct{})
	// acquiredCh is closed by the first task once it has acquired the semaphore
	// slot, giving the test a reliable signal without sleeping.
	acquiredCh := make(chan struct{})
	var acquiredOnce sync.Once

	fakeExec := &fakeExecutor{
		fn: func() {
			// Signal that the semaphore slot has been acquired.
			acquiredOnce.Do(func() { close(acquiredCh) })
			// Block until test releases.
			<-holdCh
		},
	}

	eng := New(reg, fakeExec, zap.NewNop())
	eng.SetMaxConcurrentTasks(1) // Only 1 slot.

	ctx, cancel := context.WithCancel(context.Background())
	eng.shutdownMu.Lock()
	eng.shutdownCtx = ctx
	eng.shutdownMu.Unlock()

	dir := t.TempDir()
	spec1 := writeTask(t, dir, "shutdown-task-1", `return 1`, task.TriggerConfig{Manual: true})
	spec2 := writeTask(t, dir, "shutdown-task-2", `return 1`, task.TriggerConfig{Manual: true})
	_ = reg.Register(spec1)
	_ = reg.Register(spec2)

	// Fire task 1 — it will occupy the sole semaphore slot.
	if _, err := eng.fireAsync(ctx, spec1, pkgruntime.RunOptions{}, "test"); err != nil {
		t.Fatalf("fireAsync 1: %v", err)
	}

	// Wait until task 1 has acquired the semaphore before firing task 2.
	select {
	case <-acquiredCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for task 1 to acquire semaphore")
	}

	// Fire task 2 — it will block waiting for a slot.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = eng.fireAsync(ctx, spec2, pkgruntime.RunOptions{}, "test")
	}()

	// Cancel the shutdown context — task 2's goroutine should unblock.
	cancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Good: shutdown unblocked the waiting goroutine.
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not unblock the waiting task goroutine")
	}

	// Unblock task 1 so the goroutine can exit cleanly.
	close(holdCh)
}

// fakeExecutor is a minimal pkgruntime.Executor for testing.
type fakeExecutor struct {
	fn func()
}

func (f *fakeExecutor) Execute(_ context.Context, _ *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	if f.fn != nil {
		f.fn()
	}
	return &pkgruntime.RunResult{}, nil
}
