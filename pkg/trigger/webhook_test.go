package trigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// signBodyWithTimestamp produces HMAC-SHA256 over "<tsStr>\n<body>".
// Use when sending X-Dicode-Timestamp so the signature matches the new
// preimage that includes the timestamp.
func signBodyWithTimestamp(secret, tsStr string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(tsStr))
	mac.Write([]byte("\n"))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookSignature_NoSecret_Passes(t *testing.T) {
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: ""}}
	req := httptest.NewRequest("POST", "/hooks/test", nil)
	if err := verifyWebhookSignature(spec, req, []byte("body")); err != nil {
		t.Errorf("expected nil (no secret), got: %v", err)
	}
}

func TestVerifyWebhookSignature_ValidSignature_Passes(t *testing.T) {
	secret := "my-webhook-secret"
	body := []byte(`{"event":"push"}`)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}

	req := httptest.NewRequest("POST", "/hooks/test", nil)
	req.Header.Set(webhookSignatureHeader, signBody(secret, body))

	if err := verifyWebhookSignature(spec, req, body); err != nil {
		t.Errorf("expected nil for valid signature, got: %v", err)
	}
}

func TestVerifyWebhookSignature_WrongSecret_Fails(t *testing.T) {
	body := []byte(`{"event":"push"}`)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: "correct-secret"}}

	req := httptest.NewRequest("POST", "/hooks/test", nil)
	req.Header.Set(webhookSignatureHeader, signBody("wrong-secret", body))

	if err := verifyWebhookSignature(spec, req, body); err == nil {
		t.Error("expected error for wrong secret, got nil")
	}
}

func TestVerifyWebhookSignature_MissingHeader_Fails(t *testing.T) {
	body := []byte(`{"event":"push"}`)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: "secret"}}

	req := httptest.NewRequest("POST", "/hooks/test", nil)
	// No signature header set.

	if err := verifyWebhookSignature(spec, req, body); err == nil {
		t.Error("expected error for missing signature header, got nil")
	}
}

func TestVerifyWebhookSignature_ReplayProtection_Fresh_Passes(t *testing.T) {
	secret := "secret"
	body := []byte(`{}`)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}

	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest("POST", "/hooks/test", nil)
	req.Header.Set(webhookSignatureHeader, signBodyWithTimestamp(secret, tsStr, body))
	req.Header.Set(webhookTimestampHeader, tsStr)

	if err := verifyWebhookSignature(spec, req, body); err != nil {
		t.Errorf("fresh timestamp should pass, got: %v", err)
	}
}

func TestVerifyWebhookSignature_ReplayProtection_Stale_Fails(t *testing.T) {
	secret := "secret"
	body := []byte(`{}`)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}

	staleTs := time.Now().Add(-10 * time.Minute).Unix()
	staleTsStr := fmt.Sprintf("%d", staleTs)

	req := httptest.NewRequest("POST", "/hooks/test", nil)
	// Sign with ts+body so the signature itself is valid — we want the stale
	// timestamp check (not the signature check) to be what rejects this request.
	req.Header.Set(webhookSignatureHeader, signBodyWithTimestamp(secret, staleTsStr, body))
	req.Header.Set(webhookTimestampHeader, staleTsStr)

	if err := verifyWebhookSignature(spec, req, body); err == nil {
		t.Error("stale timestamp should fail, got nil")
	}
}

func TestVerifyWebhookSignature_InvalidTimestamp_Fails(t *testing.T) {
	secret := "secret"
	body := []byte(`{}`)
	spec := &task.Spec{Trigger: task.TriggerConfig{WebhookSecret: secret}}

	req := httptest.NewRequest("POST", "/hooks/test", nil)
	req.Header.Set(webhookSignatureHeader, signBody(secret, body))
	req.Header.Set(webhookTimestampHeader, "not-a-number")

	if err := verifyWebhookSignature(spec, req, body); err == nil {
		t.Error("invalid timestamp should fail, got nil")
	}
}
