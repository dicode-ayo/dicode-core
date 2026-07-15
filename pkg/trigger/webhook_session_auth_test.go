package trigger

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/task"
)

// TestWebhookHandler_SessionAuthSkipsSignature verifies that a request marked
// session-authenticated (ipc.WithSessionAuth) bypasses HMAC signature
// verification on a secret-protected webhook, while an unmarked request with no
// signature is still rejected.
func TestWebhookHandler_SessionAuthSkipsSignature(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "sec-hook",
		`export default async function main() { return "ran" }`,
		task.TriggerConfig{Webhook: "/hooks/sec", WebhookAuth: task.WebhookAuthAny, WebhookSecret: "s3cr3t"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)
	handler := e.engine.WebhookHandler()

	// No signature, not session-authed → HMAC rejects with 403.
	req := httptest.NewRequest(http.MethodPost, "/hooks/sec", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unsigned + no session: expected 403, got %d: %s", w.Code, w.Body.String())
	}

	// No signature, session-authed → verification skipped, task runs.
	req = httptest.NewRequest(http.MethodPost, "/hooks/sec", strings.NewReader(`{}`))
	req = req.WithContext(ipc.WithSessionAuth(req.Context()))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unsigned + session: expected 200 (verification skipped), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ran") {
		t.Errorf("expected task to run, got: %s", w.Body.String())
	}
}

// TestWebhookHandler_SessionAuthSkipsReplay verifies that two identical
// session-authed submissions both succeed — the replay cache (which keys on the
// body) must not 409 a browser that legitimately submits the same body twice.
func TestWebhookHandler_SessionAuthSkipsReplay(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "replay-hook",
		`export default async function main() { return "ran" }`,
		task.TriggerConfig{Webhook: "/hooks/replay", WebhookAuth: task.WebhookAuthAny, WebhookSecret: "s3cr3t"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)
	handler := e.engine.WebhookHandler()

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/hooks/replay", strings.NewReader(`{"same":"body"}`))
		req = req.WithContext(ipc.WithSessionAuth(req.Context()))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("submission %d: expected 200, got %d: %s", i, w.Code, w.Body.String())
		}
	}
}
