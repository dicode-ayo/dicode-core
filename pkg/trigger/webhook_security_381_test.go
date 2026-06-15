package trigger

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// Fix 1: GET with webhook_secret must be rejected.
func TestVerifyWebhookSignature_GetWithSecret_Rejected(t *testing.T) {
	secret := "my-secret"
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}
	req := httptest.NewRequest(http.MethodGet, "/hooks/test?foo=bar", nil)
	req.Header.Set(webhookSignatureHeader, signBody(secret, nil))
	if err := verifyWebhookSignature(spec, req, nil); err == nil {
		t.Error("GET with webhook_secret must be rejected, got nil")
	}
}

// GET without a secret is still allowed (open webhook, used for asset serving).
func TestVerifyWebhookSignature_GetWithoutSecret_Allowed(t *testing.T) {
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: ""}}
	req := httptest.NewRequest(http.MethodGet, "/hooks/test", nil)
	if err := verifyWebhookSignature(spec, req, nil); err != nil {
		t.Errorf("GET without secret must pass: %v", err)
	}
}

// Fix 3: Timestamp in HMAC — correct signature (ts + "\n" + body) must pass.
func TestVerifyWebhookSignature_TimestampInHMAC_CorrectSig_Passes(t *testing.T) {
	secret := "ts-secret"
	body := []byte(`{"event":"push"}`)
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}

	req := httptest.NewRequest(http.MethodPost, "/hooks/ts-test", nil)
	req.Header.Set(webhookTimestampHeader, tsStr)
	req.Header.Set(webhookSignatureHeader, signBodyWithTimestamp(secret, tsStr, body))

	if err := verifyWebhookSignature(spec, req, body); err != nil {
		t.Errorf("correct ts+body signature should pass: %v", err)
	}
}

// Fix 3: A signature over body-only must fail when a timestamp is present.
func TestVerifyWebhookSignature_TimestampPresent_BodyOnlySig_Fails(t *testing.T) {
	secret := "ts-secret"
	body := []byte(`{"event":"push"}`)
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}

	req := httptest.NewRequest(http.MethodPost, "/hooks/ts-fail", nil)
	req.Header.Set(webhookTimestampHeader, tsStr)
	// Wrong: signing body only, not ts + "\n" + body.
	req.Header.Set(webhookSignatureHeader, signBody(secret, body))

	if err := verifyWebhookSignature(spec, req, body); err == nil {
		t.Error("body-only sig with timestamp present should fail; got nil")
	}
}

// Without timestamp, body-only signature still works (backwards-compat).
func TestVerifyWebhookSignature_NoTimestamp_BodyOnlySig_Passes(t *testing.T) {
	secret := "notimestamp-secret"
	body := []byte(`{"event":"push"}`)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}

	req := httptest.NewRequest(http.MethodPost, "/hooks/notstest", nil)
	req.Header.Set(webhookSignatureHeader, signBody(secret, body))
	// No X-Dicode-Timestamp header.

	if err := verifyWebhookSignature(spec, req, body); err != nil {
		t.Errorf("body-only sig without timestamp should still work: %v", err)
	}
}

// Fix 4: A second task trying to register the same webhook path is rejected.
func TestRegisterWebhookPath_DuplicatePath_Rejected(t *testing.T) {
	e := newTestEnv(t)

	e.engine.registerWebhookPath("task-a", "/hooks/shared")

	e.engine.mu.Lock()
	got := e.engine.webhooks["/hooks/shared"]
	e.engine.mu.Unlock()
	if got != "task-a" {
		t.Fatalf("first registration should succeed, got %q", got)
	}

	// Second task tries to claim the same path.
	e.engine.registerWebhookPath("task-b", "/hooks/shared")

	e.engine.mu.Lock()
	got = e.engine.webhooks["/hooks/shared"]
	e.engine.mu.Unlock()
	if got != "task-a" {
		t.Errorf("duplicate registration should be rejected; owner is %q, want task-a", got)
	}
}

// A task re-registering its own path (e.g. on reconciler reload) must succeed.
func TestRegisterWebhookPath_SameTaskReregisters_Allowed(t *testing.T) {
	e := newTestEnv(t)

	e.engine.registerWebhookPath("task-a", "/hooks/mypath")
	e.engine.registerWebhookPath("task-a", "/hooks/mypath") // re-register same task

	e.engine.mu.Lock()
	got := e.engine.webhooks["/hooks/mypath"]
	e.engine.mu.Unlock()
	if got != "task-a" {
		t.Errorf("same-task re-registration should succeed, got %q", got)
	}
}

// Fix 2: MaxConcurrentTasks must be enforced on the sync (default webhook) path.
func TestWebhookHandler_ConcurrencyLimit_Returns503(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	// Configure the semaphore with 1 slot and fill it so the webhook handler
	// finds no capacity. SetMaxConcurrentTasks must be called before Start().
	e.engine.SetMaxConcurrentTasks(1)
	e.engine.taskSem <- struct{}{} // occupy the only slot

	spec := writeTask(t, dir, "concurrency-test",
		`export default async function main() { return "ok" }`,
		task.TriggerConfig{Webhook: "/hooks/concurrency-test"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	handler := e.engine.WebhookHandler()
	req := httptest.NewRequest(http.MethodPost, "/hooks/concurrency-test", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Drain the slot we placed so t.Cleanup doesn't deadlock.
	select {
	case <-e.engine.taskSem:
	default:
	}

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("at-capacity webhook should return 503, got %d: %s", w.Code, w.Body.String())
	}
}
