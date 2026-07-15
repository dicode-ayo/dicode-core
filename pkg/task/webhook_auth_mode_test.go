package task

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWebhookAuthMode_MarshalJSON pins the backward-stable wire format that
// keeps approval content hashes unchanged for existing configs: none and
// session encode exactly as the old bool field (false/true), and only any
// diverges — so switching a task to any re-pends it while a plain auth: true
// task does not spuriously re-pend on upgrade.
func TestWebhookAuthMode_MarshalJSON(t *testing.T) {
	cases := map[WebhookAuthMode]string{
		WebhookAuthNone:    `false`,
		WebhookAuthSession: `true`,
		WebhookAuthAny:     `"any"`,
	}
	for mode, want := range cases {
		b, err := json.Marshal(mode)
		if err != nil {
			t.Fatalf("marshal %q: %v", mode, err)
		}
		if string(b) != want {
			t.Errorf("marshal %q = %s, want %s", mode, b, want)
		}
	}
}

func TestWebhookAuthMode_UnmarshalYAML(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		want    WebhookAuthMode
		wantErr bool
	}{
		{"bool true → session", "auth: true", WebhookAuthSession, false},
		{"bool false → none", "auth: false", WebhookAuthNone, false},
		{"string session", `auth: "session"`, WebhookAuthSession, false},
		{"string any", `auth: "any"`, WebhookAuthAny, false},
		{"empty string → none", `auth: ""`, WebhookAuthNone, false},
		{"absent → none", "webhook: /hooks/x", WebhookAuthNone, false},
		{"invalid string", `auth: "always"`, WebhookAuthNone, true},
		{"invalid int", "auth: 3", WebhookAuthNone, true},
		{"invalid mapping", "auth: {mode: any}", WebhookAuthNone, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr TriggerConfig
			err := yaml.Unmarshal([]byte(tc.yaml), &tr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got mode %q", tr.WebhookAuth)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tr.WebhookAuth != tc.want {
				t.Errorf("got %q, want %q", tr.WebhookAuth, tc.want)
			}
		})
	}
}

func TestWebhookAuthMode_Predicates(t *testing.T) {
	cases := []struct {
		mode            WebhookAuthMode
		enabled         bool
		requiresSession bool
	}{
		{WebhookAuthNone, false, false},
		{WebhookAuthSession, true, true},
		{WebhookAuthAny, true, false},
	}
	for _, tc := range cases {
		if got := tc.mode.Enabled(); got != tc.enabled {
			t.Errorf("%q.Enabled() = %v, want %v", tc.mode, got, tc.enabled)
		}
		if got := tc.mode.RequiresSession(); got != tc.requiresSession {
			t.Errorf("%q.RequiresSession() = %v, want %v", tc.mode, got, tc.requiresSession)
		}
	}
}

func TestValidate_AnyRequiresSecret(t *testing.T) {
	base := func() *Spec {
		return &Spec{
			Name:    "t",
			Runtime: RuntimeDeno,
			Trigger: TriggerConfig{Webhook: "/hooks/x"},
		}
	}

	t.Run("any without secret errors", func(t *testing.T) {
		s := base()
		s.Trigger.WebhookAuth = WebhookAuthAny
		if err := s.Validate(); err == nil {
			t.Fatal("expected error for auth: any without webhook_secret")
		}
	})
	t.Run("any with secret ok", func(t *testing.T) {
		s := base()
		s.Trigger.WebhookAuth = WebhookAuthAny
		s.Trigger.WebhookSecret = "s3cr3t"
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("session without secret ok", func(t *testing.T) {
		s := base()
		s.Trigger.WebhookAuth = WebhookAuthSession
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
