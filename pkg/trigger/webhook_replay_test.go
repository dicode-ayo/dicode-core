package trigger

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// TestWebhookReplay_SecondRequestRejected verifies that a second POST with the
// same body and valid HMAC is rejected with 409 (duplicate webhook).
func TestWebhookReplay_SecondRequestRejected(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	spec := writeTask(t, dir, "replay-test", `export default async function main() { return "ok" }`, task.TriggerConfig{
		Webhook:       "/hooks/replay-test",
		WebhookSecret: "test-secret",
	})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	handler := e.engine.WebhookHandler()
	body := `{"event":"push"}`
	sig := signBody("test-secret", []byte(body))

	// First request — should not be 409.
	req1 := httptest.NewRequest(http.MethodPost, "/hooks/replay-test", strings.NewReader(body))
	req1.Header.Set(webhookSignatureHeader, sig)
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	if w1.Code == http.StatusConflict {
		t.Fatalf("first request should not be rejected as replay, got 409")
	}

	// Second request with identical body — must be 409.
	req2 := httptest.NewRequest(http.MethodPost, "/hooks/replay-test", strings.NewReader(body))
	req2.Header.Set(webhookSignatureHeader, sig)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Errorf("second identical request should be rejected with 409, got %d: %s", w2.Code, w2.Body.String())
	}
}

// TestWebhookReplay_OptOutAllowsDuplicates verifies that setting
// replay_protection: false allows the same body to be sent multiple times.
func TestWebhookReplay_OptOutAllowsDuplicates(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	disabled := false
	spec := writeTask(t, dir, "replay-optout", `export default async function main() { return "ok" }`, task.TriggerConfig{
		Webhook:          "/hooks/replay-optout",
		WebhookSecret:    "test-secret",
		ReplayProtection: &disabled,
	})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	handler := e.engine.WebhookHandler()
	body := `{"event":"push"}`
	sig := signBody("test-secret", []byte(body))

	for i := range 3 {
		req := httptest.NewRequest(http.MethodPost, "/hooks/replay-optout", strings.NewReader(body))
		req.Header.Set(webhookSignatureHeader, sig)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code == http.StatusConflict {
			t.Errorf("request %d should not be rejected as replay (opt-out), got 409", i+1)
		}
	}
}

// TestWebhookReplay_NoSecretNoReplayCheck verifies that open webhooks (no
// secret configured) skip the replay cache entirely and accept duplicate bodies.
func TestWebhookReplay_NoSecretNoReplayCheck(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	spec := writeTask(t, dir, "replay-open", `export default async function main() { return "ok" }`, task.TriggerConfig{
		Webhook: "/hooks/replay-open",
		// No WebhookSecret — open webhook.
	})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	handler := e.engine.WebhookHandler()
	body := `{"event":"push"}`

	for i := range 3 {
		req := httptest.NewRequest(http.MethodPost, "/hooks/replay-open", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code == http.StatusConflict {
			t.Errorf("request %d to open webhook should not be rejected as replay, got 409", i+1)
		}
	}
}
