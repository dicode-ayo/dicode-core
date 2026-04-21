package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// newAIServer builds a server with a pluggable ai.task and a gateway-registered
// echo handler so tests can verify the /api/ai/chat forward end-to-end without
// spinning up Deno.
func newAIServer(t *testing.T, aiTask string) (*Server, *ipc.Gateway) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	gw := ipc.NewGateway()
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		AI:     config.AIConfig{Task: aiTask},
	}
	srv, err := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, gw)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, gw
}

// registerEchoWebhook registers a task and a gateway handler at hookPath that
// replies with the decoded JSON body plus a canned reply. Mirrors the shape
// of a real ai-agent response (session_id + reply).
func registerEchoWebhook(t *testing.T, srv *Server, gw *ipc.Gateway, id, hookPath string) {
	t.Helper()
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Webhook: hookPath},
		Timeout: 5 * time.Second,
	}
	if err := srv.registry.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}
	gw.Register(hookPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			_ = json.Unmarshal(b, &in)
		}
		sess, _ := in["session_id"].(string)
		if sess == "" {
			sess = "generated-xyz"
		}
		resp := map[string]any{
			"session_id": sess,
			"reply":      "echo: " + toStr(in["prompt"]),
			"received":   in,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestAIChat_ReturnsReplyFromConfiguredTask is the happy path: cfg.AI.Task
// points at a registered webhook task and POST /api/ai/chat forwards the
// body through and surfaces the reply.
func TestAIChat_ReturnsReplyFromConfiguredTask(t *testing.T) {
	srv, gw := newAIServer(t, "test/echo")
	registerEchoWebhook(t, srv, gw, "test/echo", "/hooks/test/echo")

	body, _ := json.Marshal(map[string]any{
		"prompt":     "hello",
		"session_id": "abc-123",
		"task_id":    "some/task",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["reply"] != "echo: hello" {
		t.Errorf("reply = %v, want %q", got["reply"], "echo: hello")
	}
	if got["session_id"] != "abc-123" {
		t.Errorf("session_id = %v, want %q (body should pass through verbatim)", got["session_id"], "abc-123")
	}
	// Body pass-through: the echo handler re-emits the full received map.
	received, ok := got["received"].(map[string]any)
	if !ok {
		t.Fatalf("received must be a map, got %T", got["received"])
	}
	if received["task_id"] != "some/task" {
		t.Errorf("received.task_id = %v, want %q", received["task_id"], "some/task")
	}
}

// TestAIChat_MisconfiguredTask_Returns503 covers the case where cfg.AI.Task
// points at a task id that was never registered. The client did nothing wrong,
// so this is 503 (service unavailable), not 404.
func TestAIChat_MisconfiguredTask_Returns503(t *testing.T) {
	srv, _ := newAIServer(t, "nonexistent/task")

	body, _ := json.Marshal(map[string]any{"prompt": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body)
	}
	var got map[string]string
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got["error"] == "" {
		t.Error("expected structured error body")
	}
}

// TestAIChat_TaskWithoutWebhook_Returns500 covers a configured task that is
// registered but has only a cron/manual trigger — the endpoint cannot forward
// to it, so 500 with a clear message.
func TestAIChat_TaskWithoutWebhook_Returns500(t *testing.T) {
	srv, _ := newAIServer(t, "test/cronly")

	// Register a task with a cron trigger only.
	spec := &task.Spec{
		ID:      "test/cronly",
		Name:    "test/cronly",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Cron: "0 9 * * *"},
		Timeout: 5 * time.Second,
	}
	if err := srv.registry.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"prompt": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body)
	}
}

// TestAIChat_RequiresAuth verifies /api/ai/chat is gated by requireAuth when
// server.auth is enabled (i.e. it is NOT in isPublicPath).
func TestAIChat_RequiresAuth(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	body, _ := json.Marshal(map[string]any{"prompt": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without session, got %d", w.Code)
	}
	// Shape-check the body so a future refactor that accidentally takes
	// /api/ai/chat out of the authenticated group doesn't pass this test
	// via some other incidental 401 (e.g. a handler that required state
	// the test never set up).
	if !strings.Contains(w.Body.String(), "unauthorized") {
		t.Errorf("expected unauthorized body from requireAuth, got %q", w.Body.String())
	}
}

// TestAIChat_RejectsNonHookWebhook guards against /api/ai/chat being used as
// an authenticated proxy to arbitrary /api routes — and against infinite
// self-dispatch if a misconfiguration points ai.task at a task whose webhook
// is /api/ai/chat itself. Only /hooks/-prefixed webhooks are forwardable.
func TestAIChat_RejectsNonHookWebhook(t *testing.T) {
	srv, _ := newAIServer(t, "bad/task")
	if err := srv.registry.Register(&task.Spec{
		ID:      "bad/task",
		Trigger: task.TriggerConfig{Webhook: "/api/ai/chat"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"prompt": "loop me"})
	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	cookie := login(t, srv.Handler(), "hunter2", false)
	if cookie == nil {
		t.Fatal("login failed")
	}
	req.AddCookie(cookie)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for non-/hooks/ webhook, got %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "/hooks/") {
		t.Errorf("error body should name the /hooks/ prefix requirement, got %q", w.Body.String())
	}
}

// TestAIChat_RequiresPrompt rejects bodies missing prompt upfront so the
// configured task never sees an obviously-malformed request.
func TestAIChat_RequiresPrompt(t *testing.T) {
	srv, gw := newAIServer(t, "test/echo")
	registerEchoWebhook(t, srv, gw, "test/echo", "/hooks/test/echo")

	body, _ := json.Marshal(map[string]any{"session_id": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body)
	}
}

// TestAISettings_SaveAndReadBack verifies the /api/settings/ai handler:
// accept a task that exists with a /hooks/ webhook, reject everything else,
// and reflect the change in /api/config JSON.
func TestAISettings_SaveAndReadBack(t *testing.T) {
	srv, gw := newAIServer(t, "buildin/dicodai")
	registerEchoWebhook(t, srv, gw, "other/echo", "/hooks/other/echo")
	if err := srv.registry.Register(&task.Spec{
		ID:      "bad/no-webhook",
		Trigger: task.TriggerConfig{Cron: "0 0 * * *"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	save := func(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/settings/ai", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	// Reject: empty task id.
	if w := save(t, map[string]any{"task": ""}); w.Code != http.StatusBadRequest {
		t.Errorf("empty task: expected 400, got %d", w.Code)
	}
	// Reject: unregistered task.
	if w := save(t, map[string]any{"task": "does/not-exist"}); w.Code != http.StatusBadRequest {
		t.Errorf("missing task: expected 400, got %d", w.Code)
	}
	// Reject: task without a /hooks/ webhook.
	if w := save(t, map[string]any{"task": "bad/no-webhook"}); w.Code != http.StatusBadRequest {
		t.Errorf("non-webhook task: expected 400, got %d", w.Code)
	}

	// Accept: registered task with /hooks/ prefix. persistConfig needs a
	// writable dicode.yaml — point the server at a temp file first so the
	// 200 happy-path runs the full read-modify-write round-trip rather
	// than bailing out at file-write time.
	tmp := t.TempDir() + "/dicode.yaml"
	if err := os.WriteFile(tmp, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	srv.cfgPath = tmp

	if srv.cfg.AI.Task != "buildin/dicodai" {
		t.Fatalf("precondition: AI.Task = %q, want buildin/dicodai", srv.cfg.AI.Task)
	}
	if w := save(t, map[string]any{"task": "other/echo"}); w.Code != http.StatusOK {
		t.Fatalf("happy path: expected 200, got %d: %s", w.Code, w.Body)
	}
	if srv.cfg.AI.Task != "other/echo" {
		t.Errorf("cfg.AI.Task after save = %q, want other/echo", srv.cfg.AI.Task)
	}
	persisted, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if !strings.Contains(string(persisted), "other/echo") {
		t.Errorf("persisted yaml should contain the new task id:\n%s", persisted)
	}

	// Saving the default back must clean the ai: block out of the file
	// (mirrors the applyDefaults-fills-in-default contract — the file
	// shouldn't carry redundant explicit defaults).
	if err := srv.registry.Register(&task.Spec{
		ID:      "buildin/dicodai",
		Trigger: task.TriggerConfig{Webhook: "/hooks/ai/dicodai"},
	}); err != nil {
		t.Fatalf("register dicodai: %v", err)
	}
	if w := save(t, map[string]any{"task": "buildin/dicodai"}); w.Code != http.StatusOK {
		t.Fatalf("revert to default: expected 200, got %d: %s", w.Code, w.Body)
	}
	persisted, _ = os.ReadFile(tmp)
	if strings.Contains(string(persisted), "task:") {
		t.Errorf("ai.task should be absent after revert to default; got:\n%s", persisted)
	}
}

// TestAISettings_RequiresAuth confirms the endpoint is inside the
// auth-gated /api group — consistent with every other /api/settings/* handler.
func TestAISettings_RequiresAuth(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	body, _ := json.Marshal(map[string]any{"task": "buildin/dicodai"})
	req := httptest.NewRequest(http.MethodPost, "/api/settings/ai", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without session, got %d", w.Code)
	}
}
