package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
