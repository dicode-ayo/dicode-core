package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeWebhookAuth(t *testing.T) {
	cases := []struct {
		name       string
		mode       WebhookAuthMode
		secret     string
		wantMode   WebhookAuthMode
		wantSecret string
		wantWarn   bool
	}{
		{"any + real secret stays any", WebhookAuthAny, "realsecret", WebhookAuthAny, "realsecret", false},
		{"any + unresolved var downgrades", WebhookAuthAny, "${MY_SECRET}", WebhookAuthSession, "", true},
		{"any + embedded unresolved var downgrades", WebhookAuthAny, "pre-${X}", WebhookAuthSession, "", true},
		{"any + empty downgrades", WebhookAuthAny, "", WebhookAuthSession, "", true},
		{"session + unresolved var untouched", WebhookAuthSession, "${X}", WebhookAuthSession, "${X}", false},
		{"none untouched", WebhookAuthNone, "", WebhookAuthNone, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Spec{Trigger: TriggerConfig{Webhook: "/hooks/x", WebhookAuth: tc.mode, WebhookSecret: tc.secret}}
			normalizeWebhookAuth(s)
			if s.Trigger.WebhookAuth != tc.wantMode {
				t.Errorf("mode = %q, want %q", s.Trigger.WebhookAuth, tc.wantMode)
			}
			if s.Trigger.WebhookSecret != tc.wantSecret {
				t.Errorf("secret = %q, want %q", s.Trigger.WebhookSecret, tc.wantSecret)
			}
			gotWarn := len(s.Warnings) > 0
			if gotWarn != tc.wantWarn {
				t.Errorf("warning present = %v, want %v (warnings: %v)", gotWarn, tc.wantWarn, s.Warnings)
			}
		})
	}
}

// TestLoadDir_AuthAnySecretResolution exercises the full load path: an auth: any
// webhook whose secret env var is set keeps HMAC; unset, it degrades to session
// so a placeholder secret is never served.
func TestLoadDir_AuthAnySecretResolution(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, "ai-hook")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `name: ai-hook
runtime: deno
trigger:
  webhook: /hooks/ai
  auth: any
  webhook_secret: "${AI_TEST_WEBHOOK_SECRET}"
  require_timestamp: true
`
	if err := os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "task.ts"), []byte(`export default async () => "ok"`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("secret unset → session-only", func(t *testing.T) {
		os.Unsetenv("AI_TEST_WEBHOOK_SECRET")
		spec, err := LoadDir(td)
		if err != nil {
			t.Fatalf("LoadDir: %v", err)
		}
		if spec.Trigger.WebhookAuth != WebhookAuthSession {
			t.Errorf("mode = %q, want session (downgraded)", spec.Trigger.WebhookAuth)
		}
		if spec.Trigger.WebhookSecret != "" {
			t.Errorf("secret should be cleared, got %q", spec.Trigger.WebhookSecret)
		}
		found := false
		for _, w := range spec.Warnings {
			if strings.Contains(w, "session auth only") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a downgrade warning, got %v", spec.Warnings)
		}
	})

	t.Run("secret set → auth: any with HMAC", func(t *testing.T) {
		t.Setenv("AI_TEST_WEBHOOK_SECRET", "a-real-secret-value")
		spec, err := LoadDir(td)
		if err != nil {
			t.Fatalf("LoadDir: %v", err)
		}
		if spec.Trigger.WebhookAuth != WebhookAuthAny {
			t.Errorf("mode = %q, want any", spec.Trigger.WebhookAuth)
		}
		if spec.Trigger.WebhookSecret != "a-real-secret-value" {
			t.Errorf("secret = %q, want the resolved value", spec.Trigger.WebhookSecret)
		}
	})
}
