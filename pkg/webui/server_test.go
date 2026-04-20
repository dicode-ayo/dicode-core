package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
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

	srv, err := New(8080, reg, eng, &config.Config{Server: config.ServerConfig{Port: 8080}}, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
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

	// The SPA now lives at /hooks/webui; bare / redirects there.
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect to /hooks/webui, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/hooks/webui" {
		t.Fatalf("expected redirect to /hooks/webui, got %q", loc)
	}
}

func TestSPA_TaskRoute(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "detail-task", `return 1`)

	req := httptest.NewRequest(http.MethodGet, "/tasks/detail-task", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Unmatched paths redirect to /hooks/webui (SPA catch-all redirect).
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/hooks/webui" {
		t.Fatalf("expected redirect to /hooks/webui, got %q", loc)
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
	// Unmatched /runs/{id} redirects to /hooks/webui.
	if w2.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", w2.Code, w2.Body)
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
	registerWebhookTask(t, srv.registry, srv, "auth-hook", "/hooks/priv", true)

	h := srv.Handler()

	// Unauthenticated GET with browser Accept header → redirect to /login with next pointing back at the original path.
	getReq := httptest.NewRequest(http.MethodGet, "/hooks/priv", nil)
	getReq.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, getReq)
	if getW.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated browser GET: expected 303, got %d", getW.Code)
	}
	if loc := getW.Header().Get("Location"); loc != "/login?next=%2Fhooks%2Fpriv" {
		t.Errorf("unauthenticated browser GET: expected redirect to /login?next=%%2Fhooks%%2Fpriv, got %q", loc)
	}

	// Unauthenticated GET without browser Accept header → 401 JSON.
	apiGetReq := httptest.NewRequest(http.MethodGet, "/hooks/priv", nil)
	apiGetW := httptest.NewRecorder()
	h.ServeHTTP(apiGetW, apiGetReq)
	if apiGetW.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated API GET: expected 401, got %d", apiGetW.Code)
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
	registerWebhookTask(t, srv.registry, srv, "auth-hook2", "/hooks/priv2", true)

	// Issue a valid in-memory session token.
	token := srv.sessions.issue()
	h := srv.Handler()

	// GET with a valid session: should serve index.html (200), NOT be blocked.
	getReq := httptest.NewRequest(http.MethodGet, "/hooks/priv2", nil)
	getReq.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
	getReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, getReq)
	if getW.Code == http.StatusUnauthorized || getW.Code == http.StatusSeeOther {
		t.Errorf("authenticated GET: unexpected auth rejection, got %d", getW.Code)
	}

	// POST with a valid session: should pass through, NOT be blocked.
	postReq := httptest.NewRequest(http.MethodPost, "/hooks/priv2", nil)
	postReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	postW := httptest.NewRecorder()
	h.ServeHTTP(postW, postReq)
	if postW.Code == http.StatusUnauthorized || postW.Code == http.StatusSeeOther {
		t.Errorf("authenticated POST: unexpected auth rejection, got %d", postW.Code)
	}
}

func TestAPI_Metrics_OK(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if ct == "" || ct[:16] != "application/json" {
		t.Errorf("expected application/json content-type, got %q", ct)
	}
	var m map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := m["daemon"]; !ok {
		t.Error("missing 'daemon' key in metrics response")
	}
	tasks, ok := m["tasks"].(map[string]interface{})
	if !ok {
		t.Fatal("missing or non-object 'tasks' key in metrics response")
	}
	for _, key := range []string{"active_task_slots", "max_concurrent_tasks", "waiting_tasks"} {
		if _, ok := tasks[key]; !ok {
			t.Errorf("missing %q in tasks metrics response", key)
		}
	}
}

func TestAPI_Metrics_AuthRequired(t *testing.T) {
	srv, _ := newTestServer(t)
	// Enable auth so requireAuth rejects unauthenticated requests.
	srv.cfg.Server.Auth = true

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	// Mark as API request so we get 401 instead of a redirect.
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAPI_Metrics_AuthOK(t *testing.T) {
	// Verify that GET /api/metrics returns 200 with valid JSON when auth is
	// enabled and a valid session token is present.
	srv := newAuthServer(t, "hunter2")
	token := srv.sessions.issue()

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var m map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := m["daemon"]; !ok {
		t.Error("missing 'daemon' key in metrics response")
	}
	if _, ok := m["tasks"]; !ok {
		t.Error("missing 'tasks' key in metrics response")
	}
}

func TestAPI_Metrics_Concurrent(t *testing.T) {
	// Fire 5 concurrent GET /api/metrics requests and verify all return 200.
	// Run with -race to detect data races in the metrics collection path.
	srv, _ := newTestServer(t)
	h := srv.Handler()

	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("goroutine %d: expected 200, got %d", i, code)
		}
	}
}

// TestWebhookAuth_RedirectsToLoginWithNext reproduces dicode-core#96: an unauth
// browser GET to an auth-protected webhook path must redirect to /login with
// the original URI preserved in ?next=..., NOT to / (which then rolls to
// /hooks/webui, stranding the user).
func TestWebhookAuth_RedirectsToLoginWithNext(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	registerWebhookTask(t, srv.registry, srv, "ai-hook", "/hooks/ai", true)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/hooks/ai", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/login?next=%2Fhooks%2Fai" {
		t.Fatalf("expected Location=/login?next=%%2Fhooks%%2Fai, got %q", loc)
	}
	if loc == "/hooks/webui" || loc == "/" {
		t.Fatalf("bug #96 regression: unauthenticated webhook redirected to %q", loc)
	}
}

func TestWebhookAuth_RedirectsPreservesQueryAndFragment(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	registerWebhookTask(t, srv.registry, srv, "ai-hook", "/hooks/ai", true)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/hooks/ai?q=1&r=2", nil)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	loc := w.Header().Get("Location")
	if loc != "/login?next=%2Fhooks%2Fai%3Fq%3D1%26r%3D2" {
		t.Fatalf("query string not preserved in next: got %q", loc)
	}
}

func TestLoginPage_Served_WhenPathIsPublic(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !contains(body, `action="/api/auth/login"`) {
		t.Error("login form must POST to /api/auth/login")
	}
	if !contains(body, `name="password"`) {
		t.Error("login form must contain a password field")
	}
	if !contains(body, `name="next"`) {
		t.Error("login form must contain a hidden next field")
	}
}

func TestLoginPage_ShowsTaskNameForWebhookNext(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	spec := registerWebhookTask(t, srv.registry, srv, "ai-hook", "/hooks/ai", true)
	spec.Name = "AI Assistant"
	spec.Description = "Chat with your tasks"
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login?next=%2Fhooks%2Fai", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !contains(body, "AI Assistant") {
		t.Errorf("expected task name in login page, got:\n%s", body)
	}
	if !contains(body, "Chat with your tasks") {
		t.Errorf("expected task description in login page, got:\n%s", body)
	}
}

func TestLoginPage_GenericTitleForUnknownNext(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !contains(w.Body.String(), "Sign in to dicode") {
		t.Error("expected generic fallback title")
	}
}

func TestLogin_FormPost_RedirectsToNext(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	registerWebhookTask(t, srv.registry, srv, "ai-hook", "/hooks/ai", true)
	h := srv.Handler()

	form := url.Values{}
	form.Set("password", "hunter2")
	form.Set("next", "/hooks/ai")

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/hooks/ai" {
		t.Errorf("expected Location=/hooks/ai, got %q", loc)
	}
	var foundSession bool
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			foundSession = true
		}
	}
	if !foundSession {
		t.Error("expected a session cookie to be set on form login")
	}
}

func TestLogin_FormPost_WrongPassword_RendersErrorHTML(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	form := url.Values{}
	form.Set("password", "nope")
	form.Set("next", "/hooks/ai")

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected HTML response, got Content-Type=%q", ct)
	}
	if !contains(w.Body.String(), "incorrect password") {
		t.Error("expected error message in HTML body")
	}
}

func TestLogin_JSONPost_EchoesSafeNext(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	body := strings.NewReader(`{"password":"hunter2","next":"/hooks/ai"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["next"] != "/hooks/ai" {
		t.Errorf("expected next=/hooks/ai in response, got %q", resp["next"])
	}
}

func TestLogin_FormPost_OpenRedirect_Attempts_FallbackToDefault(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	abuses := []string{
		"//evil.com",
		"//evil.com/foo",
		"///evil.com",
		"https://evil.com/",
		"http://evil.com/foo",
		"javascript:alert(1)",
		"/\\evil.com",
		"/path\\with\\backslash",
		"\\/evil.com",
		"/\r\nLocation:http://evil",
	}
	for i, bad := range abuses {
		form := url.Values{}
		form.Set("password", "hunter2")
		form.Set("next", bad)

		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = fmt.Sprintf("10.9.0.%d:1234", i+1)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusSeeOther {
			t.Errorf("next=%q: expected 303 (fallback), got %d", bad, w.Code)
			continue
		}
		loc := w.Header().Get("Location")
		if loc != "/hooks/webui" {
			t.Errorf("next=%q: expected fallback to /hooks/webui, got %q (open redirect!)", bad, loc)
		}
	}
}

func TestLogin_JSONPost_OpenRedirect_Dropped(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	body := strings.NewReader(`{"password":"hunter2","next":"//evil.com/steal"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if _, ok := resp["next"]; ok {
		t.Errorf("unsafe next must not be echoed, got %q", resp["next"])
	}
}

func TestLoginPage_RejectsUnsafeNextInQueryString(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login?next=%2F%2Fevil.com", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if contains(w.Body.String(), "evil.com") {
		t.Error("unsafe next leaked into rendered login page")
	}
	if !contains(w.Body.String(), `name="next" value=""`) {
		t.Error("expected empty hidden next field when next is unsafe")
	}
}

func TestIsSafeNextPath(t *testing.T) {
	ok := []string{"/", "/hooks/ai", "/hooks/ai/sub", "/hooks/ai?x=1", "/hooks/ai#frag"}
	for _, p := range ok {
		if !isSafeNextPath(p) {
			t.Errorf("expected safe: %q", p)
		}
	}
	bad := []string{
		"", "foo", "foo/bar", "//evil.com", "///evil.com",
		"http://evil.com", "https://evil.com", "javascript:alert(1)",
		"/\\evil.com", "/foo\\bar", "\\/evil.com",
		"/foo\r\nLocation:x",
	}
	for _, p := range bad {
		if isSafeNextPath(p) {
			t.Errorf("expected unsafe: %q", p)
		}
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
