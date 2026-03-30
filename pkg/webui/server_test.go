package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

func newTestServer(t *testing.T) (*Server, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	rt, err := denoruntime.New(reg, secrets.Chain{}, d, zap.NewNop())
	if err != nil {
		t.Skipf("deno not available: %v", err)
	}
	eng := trigger.New(reg, rt, zap.NewNop())

	srv, err := New(8080, reg, eng, &config.Config{Server: config.ServerConfig{Port: 8080}}, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d)
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	return srv, reg
}

func registerTask(t *testing.T, reg *registry.Registry, id, script string) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	td := filepath.Join(dir, id)
	_ = os.MkdirAll(td, 0755)
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte("name: "+id+"\ntrigger:\n  manual: true\nruntime: deno\n"), 0644)
	_ = os.WriteFile(filepath.Join(td, "task.ts"), []byte(script), 0644)
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 5 * time.Second,
		TaskDir: td,
	}
	_ = reg.Register(spec)
	return spec
}

func TestAPI_ListTasks(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "task-a", `return 1`)
	registerTask(t, reg, "task-b", `return 2`)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var tasks []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&tasks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestAPI_GetTask(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "my-task", `return 1`)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/my-task", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
}

func TestAPI_GetTask_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/missing", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAPI_RunTask(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "run-me", `return "done"`)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/run-me/run", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["runId"] == "" {
		t.Error("expected runId in response")
	}
}

func TestAPI_RunTask_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/ghost/run", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPI_ListRuns(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "history-task", `return 1`)

	// Fire a run via the API.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/history-task/run", nil)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest(http.MethodGet, "/api/tasks/history-task/runs", nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	var runs []map[string]interface{}
	_ = json.NewDecoder(w2.Body).Decode(&runs)
	if len(runs) == 0 {
		t.Error("expected at least one run")
	}
}

func TestAPI_GetRun_and_Logs(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "log-task", `log.info("hello"); return 1`)

	// Fire a run.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/log-task/run", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	runID := resp["runId"]

	// FireManual is async; poll until run is no longer running (up to 5s).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req2 := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID, nil)
		w2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("get run: %d", w2.Code)
		}
		var run map[string]interface{}
		_ = json.NewDecoder(w2.Body).Decode(&run)
		if run["Status"] != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Get logs.
	req3 := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID+"/logs", nil)
	w3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("get logs: %d", w3.Code)
	}
	var logs []map[string]interface{}
	_ = json.NewDecoder(w3.Body).Decode(&logs)
	if len(logs) == 0 {
		t.Error("expected log entries")
	}
}

func TestSPA_Root(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// SPA index.html should be served (or 404 if static/app/index.html not embedded yet in test build)
	if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
		t.Fatalf("expected 200 or 404, got %d", w.Code)
	}
}

func TestSPA_TaskRoute(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "detail-task", `return 1`)

	req := httptest.NewRequest(http.MethodGet, "/tasks/detail-task", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// SPA catch-all should return 200 (index.html) or 404 if not yet created
	if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
		t.Fatalf("expected 200 or 404, got %d", w.Code)
	}
}

func TestSPA_RunRoute(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "rundetail-task", `return 1`)

	// Create a run.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/rundetail-task/run", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)

	req2 := httptest.NewRequest(http.MethodGet, "/runs/"+resp["runId"], nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	// SPA catch-all
	if w2.Code != http.StatusOK && w2.Code != http.StatusNotFound {
		t.Fatalf("expected 200 or 404, got %d: %s", w2.Code, w2.Body)
	}
}

func registerWebhookTask(t *testing.T, reg *registry.Registry, srv *Server, id, hookPath string, requireAuth bool) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	td := filepath.Join(dir, id)
	_ = os.MkdirAll(td, 0755)
	_ = os.WriteFile(filepath.Join(td, "task.ts"), []byte(`return "ok"`), 0644)
	// Write a minimal index.html so GET serves the UI.
	_ = os.WriteFile(filepath.Join(td, "index.html"), []byte(`<!doctype html><html><body>tool</body></html>`), 0644)
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Webhook: hookPath, WebhookAuth: requireAuth},
		Timeout: 5 * time.Second,
		TaskDir: td,
	}
	_ = reg.Register(spec)
	srv.engine.Register(spec)
	return spec
}

func TestWebhook_PublicTask_NoAuth(t *testing.T) {
	srv, reg := newTestServer(t)
	registerWebhookTask(t, reg, srv, "pub-hook", "/hooks/pub", false)

	// GET and POST without a session should not be blocked by session auth.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "/hooks/pub", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusSeeOther {
			t.Errorf("public webhook %s: should not require auth, got %d", method, w.Code)
		}
	}
}

func TestWebhook_AuthTask_BlocksUnauthenticated(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	dir := t.TempDir()
	td := filepath.Join(dir, "auth-hook")
	_ = os.MkdirAll(td, 0755)
	_ = os.WriteFile(filepath.Join(td, "task.ts"), []byte(`return "ok"`), 0644)
	_ = os.WriteFile(filepath.Join(td, "index.html"), []byte(`<!doctype html><html><body>tool</body></html>`), 0644)
	spec := &task.Spec{
		ID:      "auth-hook",
		Name:    "auth-hook",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Webhook: "/hooks/priv", WebhookAuth: true},
		Timeout: 5 * time.Second,
		TaskDir: td,
	}
	_ = srv.registry.Register(spec)
	srv.engine.Register(spec)

	h := srv.Handler()

	// Unauthenticated GET → redirect to /?auth=required.
	getReq := httptest.NewRequest(http.MethodGet, "/hooks/priv", nil)
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, getReq)
	if getW.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated GET: expected 303, got %d", getW.Code)
	}
	if loc := getW.Header().Get("Location"); loc != "/?auth=required" {
		t.Errorf("unauthenticated GET: expected redirect to /?auth=required, got %q", loc)
	}

	// Unauthenticated POST → 401 JSON.
	postReq := httptest.NewRequest(http.MethodPost, "/hooks/priv", nil)
	postW := httptest.NewRecorder()
	h.ServeHTTP(postW, postReq)
	if postW.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST: expected 401, got %d", postW.Code)
	}
}

func TestWebhook_AuthTask_AllowsAuthenticatedSession(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	dir := t.TempDir()
	td := filepath.Join(dir, "auth-hook2")
	_ = os.MkdirAll(td, 0755)
	_ = os.WriteFile(filepath.Join(td, "index.html"), []byte(`<!doctype html><html><body>tool</body></html>`), 0644)
	spec := &task.Spec{
		ID:      "auth-hook2",
		Name:    "auth-hook2",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Webhook: "/hooks/priv2", WebhookAuth: true},
		Timeout: 5 * time.Second,
		TaskDir: td,
	}
	_ = srv.registry.Register(spec)
	srv.engine.Register(spec)

	// Issue a valid in-memory session token.
	token := srv.sessions.issue()
	h := srv.Handler()

	// GET with a valid session: should serve index.html (200), NOT be blocked.
	getReq := httptest.NewRequest(http.MethodGet, "/hooks/priv2", nil)
	getReq.AddCookie(&http.Cookie{Name: secretsCookie, Value: token})
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, getReq)
	if getW.Code == http.StatusUnauthorized || getW.Code == http.StatusSeeOther {
		t.Errorf("authenticated GET: unexpected auth rejection, got %d", getW.Code)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
