package trigger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

// Fix 1: GET with webhook_secret must be rejected.
func TestVerifyWebhookSignature_GetWithSecret_Rejected(t *testing.T) {
	secret := "my-secret"
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}
	req := httptest.NewRequest(http.MethodGet, "/hooks/test?foo=bar", nil)
	req.Header.Set(webhookSignatureHeader, signBody(secret, nil))
	if _, _, err := verifyWebhookSignature(spec, req, nil); err == nil {
		t.Error("GET with webhook_secret must be rejected, got nil")
	}
}

// GET without a secret is still allowed (open webhook, used for asset serving).
func TestVerifyWebhookSignature_GetWithoutSecret_Allowed(t *testing.T) {
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: ""}}
	req := httptest.NewRequest(http.MethodGet, "/hooks/test", nil)
	if _, _, err := verifyWebhookSignature(spec, req, nil); err != nil {
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

	if _, _, err := verifyWebhookSignature(spec, req, body); err != nil {
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

	if _, _, err := verifyWebhookSignature(spec, req, body); err == nil {
		t.Error("body-only sig with timestamp present should fail; got nil")
	}
}

// Without timestamp, body-only signature still works (backwards-compat — this
// is what makes the endpoint GitHub-compatible, since GitHub never sends
// X-Dicode-Timestamp and cannot be made to).
func TestVerifyWebhookSignature_NoTimestamp_BodyOnlySig_Passes(t *testing.T) {
	secret := "notimestamp-secret"
	body := []byte(`{"event":"push"}`)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}

	req := httptest.NewRequest(http.MethodPost, "/hooks/notstest", nil)
	req.Header.Set(webhookSignatureHeader, signBody(secret, body))
	// No X-Dicode-Timestamp header.

	if _, _, err := verifyWebhookSignature(spec, req, body); err != nil {
		t.Errorf("body-only sig without timestamp should still work: %v", err)
	}
}

// #605 fix 1: trigger.require_timestamp=true rejects a request with no
// X-Dicode-Timestamp header, even though a valid body-only signature would
// otherwise be accepted (the GitHub-compat path above).
func TestVerifyWebhookSignature_RequireTimestamp_MissingHeader_Fails(t *testing.T) {
	secret := "require-ts-secret"
	body := []byte(`{"event":"push"}`)
	requireTS := true
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret, RequireTimestamp: &requireTS}}

	req := httptest.NewRequest(http.MethodPost, "/hooks/require-ts", nil)
	req.Header.Set(webhookSignatureHeader, signBody(secret, body))
	// No X-Dicode-Timestamp header — must be rejected when require_timestamp is set.

	if _, _, err := verifyWebhookSignature(spec, req, body); err == nil {
		t.Error("missing timestamp with require_timestamp=true should fail; got nil")
	}
}

// #605 fix 1: trigger.require_timestamp=true still accepts a correctly
// ts+body-signed request.
func TestVerifyWebhookSignature_RequireTimestamp_WithHeader_Passes(t *testing.T) {
	secret := "require-ts-secret"
	body := []byte(`{"event":"push"}`)
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	requireTS := true
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret, RequireTimestamp: &requireTS}}

	req := httptest.NewRequest(http.MethodPost, "/hooks/require-ts", nil)
	req.Header.Set(webhookTimestampHeader, tsStr)
	req.Header.Set(webhookSignatureHeader, signBodyWithTimestamp(secret, tsStr, body))

	if _, _, err := verifyWebhookSignature(spec, req, body); err != nil {
		t.Errorf("ts+body signature with require_timestamp=true should pass: %v", err)
	}
}

// #605 fix 2: the replay cache must key on (timestamp, body), not body alone —
// two legitimate requests with an identical body but distinct, individually
// valid timestamps must not collide as a spurious replay.
func TestCheckWebhookReplay_DistinctTimestamps_SameBody_BothAccepted(t *testing.T) {
	e := &Engine{webhookReplayCache: newReplayCache(time.Hour)}
	secret := "replay-secret"
	body := []byte(`{"event":"push"}`)

	ts1 := strconv.FormatInt(time.Now().Unix(), 10)
	ts2 := strconv.FormatInt(time.Now().Unix()+1, 10)

	if err := e.checkWebhookReplay(secret, nil, webhookHMACPreimageDigest(secret, ts1, body)); err != nil {
		t.Fatalf("first (ts1, body) pair should be accepted: %v", err)
	}
	if err := e.checkWebhookReplay(secret, nil, webhookHMACPreimageDigest(secret, ts2, body)); err != nil {
		t.Errorf("second request with a distinct timestamp over the same body must not be treated as a replay: %v", err)
	}
}

// #605 fix 2: replaying the exact same (timestamp, body) pair is still rejected.
func TestCheckWebhookReplay_SameTimestampAndBody_Rejected(t *testing.T) {
	e := &Engine{webhookReplayCache: newReplayCache(time.Hour)}
	secret := "replay-secret"
	body := []byte(`{"event":"push"}`)
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)

	digest := webhookHMACPreimageDigest(secret, tsStr, body)
	if err := e.checkWebhookReplay(secret, nil, digest); err != nil {
		t.Fatalf("first (ts, body) pair should be accepted: %v", err)
	}
	if err := e.checkWebhookReplay(secret, nil, digest); err == nil {
		t.Error("replaying the exact same (ts, body) pair should be rejected")
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

// Fix 2 (regression): a webhook run that triggers a nested synchronous run
// (if_missing prereq / input-storage delegation, both routed through fireSync
// from inside the executing task) must not self-block on the parent's own
// concurrency slot. With the cap saturated by the parent, the nested fireSync
// must still succeed — the gate lives at the webhook entry, not in fireSync.
func TestFireSync_NestedRun_NotBlockedBySlot(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	dir := t.TempDir()
	child := writeTask(t, dir, "nested-child", `return 1`, task.TriggerConfig{Manual: true})

	var eng *Engine
	var nestedErr error
	var nestedRan bool
	firedNested := false
	exec := &fakeExecutor{fn: func() {
		// Fire the nested child only on the parent's first execution; the
		// child run reuses this same executor and must just return, or the
		// fn would recurse forever.
		if firedNested {
			return
		}
		firedNested = true
		// Runs inside the parent's executor while the parent holds the only
		// slot. Firing the child synchronously must not deadlock or 503.
		nestedRan = true
		_, _, nestedErr = eng.fireSync(context.Background(), child, pkgruntime.RunOptions{}, "if_missing")
	}}
	eng = New(reg, exec, zap.NewNop())

	eng.SetMaxConcurrentTasks(1)
	_ = reg.Register(child)
	eng.Register(child)

	parent := writeTask(t, dir, "nested-parent", `return 1`, task.TriggerConfig{Webhook: "/hooks/nested-parent"})
	_ = reg.Register(parent)
	eng.Register(parent)

	handler := eng.WebhookHandler()
	req := httptest.NewRequest(http.MethodPost, "/hooks/nested-parent", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !nestedRan {
		t.Fatal("parent executor never ran")
	}
	if nestedErr != nil {
		t.Errorf("nested fireSync must not be blocked by parent's slot, got: %v", nestedErr)
	}
	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("parent webhook should not 503 when only the parent occupies the slot")
	}
}
