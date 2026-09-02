package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeParams_RedactsByName(t *testing.T) {
	got := SanitizeParams(map[string]string{
		"api_key":           "sk-live-12345",
		"GITHUB_TOKEN":      "ghp_abcdef",
		"slack_secret":      "xoxb-1111",
		"password":          "hunter2",
		"webhook_signature": "sha256=deadbeef",
		"channel":           "#alerts",
	})

	var m map[string]string
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	for _, k := range []string{"api_key", "GITHUB_TOKEN", "slack_secret", "password", "webhook_signature"} {
		if m[k] != Redacted {
			t.Errorf("key %q: got %q, want %q", k, m[k], Redacted)
		}
	}
	if m["channel"] != "#alerts" {
		t.Errorf("non-sensitive key was altered: got %q", m["channel"])
	}
	for _, leaked := range []string{"sk-live-12345", "ghp_abcdef", "xoxb-1111", "hunter2", "deadbeef"} {
		if strings.Contains(got, leaked) {
			t.Errorf("sanitized output leaks secret value %q: %s", leaked, got)
		}
	}
}

// Every run audits its params through this list (pkg/trigger/run.go),
// including the approval hook's own notify_task fire, so an unredacted
// approve_url puts a working approval link in the audit trail.
func TestSanitizeParams_RedactsApproveURL(t *testing.T) {
	got := SanitizeParams(map[string]string{
		"task_id":     "repo/pending-task",
		"hash":        "abc123",
		"approve_url": "https://host/approve/tok-secret-xyz",
	})

	var m map[string]string
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	if m["approve_url"] != Redacted {
		t.Errorf("approve_url: got %q, want %q", m["approve_url"], Redacted)
	}
	if m["task_id"] != "repo/pending-task" {
		t.Errorf("task_id should not be redacted: got %q", m["task_id"])
	}
	if strings.Contains(got, "tok-secret-xyz") {
		t.Errorf("sanitized output leaks the approval token: %s", got)
	}
}

func TestSanitizeParams_RedactsEnvAndSecretRefs(t *testing.T) {
	got := SanitizeParams(map[string]string{
		"target":  "env:GH_TOKEN",
		"webhook": "secret:slack_webhook_url",
		"other":   "secrets:db_password",
		"mixed":   "  ENV:UPPER_CASE_REF",
		"plain":   "environment-name", // does not start with env: — must survive
	})

	var m map[string]string
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	for _, k := range []string{"target", "webhook", "other", "mixed"} {
		if m[k] != Redacted {
			t.Errorf("key %q: got %q, want %q", k, m[k], Redacted)
		}
	}
	if m["plain"] != "environment-name" {
		t.Errorf("plain value was altered: got %q", m["plain"])
	}
}

func TestSanitizeParams_Empty(t *testing.T) {
	if got := SanitizeParams(nil); got != "" {
		t.Errorf("nil map: got %q, want empty string", got)
	}
	if got := SanitizeParams(map[string]string{}); got != "" {
		t.Errorf("empty map: got %q, want empty string", got)
	}
}

func TestSanitizeAny_Nested(t *testing.T) {
	got := SanitizeAny(map[string]any{
		"query": "list issues",
		"auth": map[string]any{
			"token": "ghp_secretvalue",
		},
		"items": []any{
			map[string]any{"apiKey": "sk-12345", "name": "ok"},
			"env:SOME_VAR",
			float64(42),
		},
	})

	if strings.Contains(got, "ghp_secretvalue") || strings.Contains(got, "sk-12345") {
		t.Fatalf("nested secret leaked: %s", got)
	}
	if strings.Contains(got, "env:SOME_VAR") {
		t.Fatalf("env ref leaked: %s", got)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	if m["query"] != "list issues" {
		t.Errorf("non-sensitive value altered: %v", m["query"])
	}
	items := m["items"].([]any)
	if items[1] != Redacted {
		t.Errorf("string ref in list: got %v, want %q", items[1], Redacted)
	}
	if items[2] != float64(42) {
		t.Errorf("number in list altered: %v", items[2])
	}
}

func TestSanitizeAny_DepthCap(t *testing.T) {
	// Build a structure deeper than maxSanitizeDepth — must not panic.
	v := any("leaf")
	for i := 0; i < maxSanitizeDepth+10; i++ {
		v = map[string]any{"nested": v}
	}
	got := SanitizeAny(v)
	if got == "" {
		t.Fatal("expected non-empty output for deep structure")
	}
	if !strings.Contains(got, Redacted) {
		t.Errorf("expected depth cap to redact the innermost levels: %s", got)
	}
}

func TestSanitizeAny_Nil(t *testing.T) {
	if got := SanitizeAny(nil); got != "" {
		t.Errorf("nil: got %q, want empty string", got)
	}
}
