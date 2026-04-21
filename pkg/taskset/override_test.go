package taskset

import (
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

func boolPtr(b bool) *bool                  { return &b }
func strPtr(s string) *string               { return &s }
func durPtr(d time.Duration) *time.Duration { return &d }

func baseSpec() *task.Spec {
	return &task.Spec{
		Name:    "test",
		Runtime: task.RuntimeDeno,
		Timeout: 60 * time.Second,
		Trigger: task.TriggerConfig{Cron: "0 8 * * *"},
		Permissions: task.Permissions{
			Env: []task.EnvEntry{
				{Name: "APP", Value: "base"},
				{Name: "LOG", Value: "debug"},
			},
		},
		Params: task.Params{
			{Name: "env", Default: "staging"},
			{Name: "region", Default: "us-east-1"},
		},
	}
}

// envMap converts permissions.env entries to a name→value map for assertions.
func envMap(entries []task.EnvEntry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		v := e.Value
		if v == "" && e.Secret != "" {
			v = "secret:" + e.Secret
		}
		m[e.Name] = v
	}
	return m
}

// ── mergeEnvEntries ───────────────────────────────────────────────────────────

func TestMergeEnvEntries_NewKeyAppended(t *testing.T) {
	base := []task.EnvEntry{{Name: "A", Value: "1"}}
	overlay := []task.EnvEntry{{Name: "B", Value: "2"}}
	got := mergeEnvEntries(base, overlay)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %v", got)
	}
	if got[0].Name != "A" || got[1].Name != "B" {
		t.Errorf("unexpected: %v", got)
	}
}

func TestMergeEnvEntries_OverlayWins(t *testing.T) {
	base := []task.EnvEntry{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}
	overlay := []task.EnvEntry{{Name: "A", Value: "99"}}
	got := mergeEnvEntries(base, overlay)
	em := envMap(got)
	if em["A"] != "99" {
		t.Errorf("A should be 99, got %q", em["A"])
	}
	if em["B"] != "2" {
		t.Errorf("B should be 2, got %q", em["B"])
	}
}

func TestMergeEnvEntries_PreservesOrder(t *testing.T) {
	base := []task.EnvEntry{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}, {Name: "C", Value: "3"}}
	overlay := []task.EnvEntry{{Name: "B", Value: "99"}, {Name: "D", Value: "4"}}
	got := mergeEnvEntries(base, overlay)
	if len(got) != 4 {
		t.Fatalf("want 4, got %v", got)
	}
	if got[0].Name != "A" || got[1].Name != "B" || got[2].Name != "C" || got[3].Name != "D" {
		t.Errorf("order wrong: %v", got)
	}
}

func TestMergeEnvEntries_SecretEntry(t *testing.T) {
	base := []task.EnvEntry{{Name: "TOKEN", Secret: "my_token"}}
	overlay := []task.EnvEntry{{Name: "TOKEN", Secret: "new_token", Optional: true}}
	got := mergeEnvEntries(base, overlay)
	if len(got) != 1 || got[0].Secret != "new_token" || !got[0].Optional {
		t.Errorf("unexpected: %v", got)
	}
}

// ── mergeParams ───────────────────────────────────────────────────────────────

func TestMergeParams_PatchExisting(t *testing.T) {
	params := task.Params{{Name: "env", Default: "staging"}}
	mergeParams(&params, []ParamOverride{{Name: "env", Default: "production"}})
	if params[0].Default != "production" {
		t.Errorf("got %q", params[0].Default)
	}
}

func TestMergeParams_AppendNew(t *testing.T) {
	params := task.Params{{Name: "env", Default: "staging"}}
	mergeParams(&params, []ParamOverride{{Name: "region", Default: "eu-west-1"}})
	if len(params) != 2 {
		t.Fatalf("want 2 params, got %d", len(params))
	}
	if params[1].Name != "region" || params[1].Default != "eu-west-1" {
		t.Errorf("wrong new param: %+v", params[1])
	}
}

func TestMergeParams_PatchRequired(t *testing.T) {
	params := task.Params{{Name: "env", Required: false}}
	mergeParams(&params, []ParamOverride{{Name: "env", Required: boolPtr(true)}})
	if !params[0].Required {
		t.Error("required should be true")
	}
}

// ── applyTriggerPatch ─────────────────────────────────────────────────────────

func TestApplyTriggerPatch_Cron(t *testing.T) {
	tr := task.TriggerConfig{Cron: "0 8 * * *"}
	applyTriggerPatch(&tr, &TriggerPatch{Cron: strPtr("0 3 * * *")})
	if tr.Cron != "0 3 * * *" {
		t.Errorf("got %q", tr.Cron)
	}
	// Other trigger types must be cleared.
	if tr.Webhook != "" || tr.Manual || tr.Daemon || tr.Chain != nil {
		t.Error("other trigger types not cleared")
	}
}

func TestApplyTriggerPatch_Daemon(t *testing.T) {
	tr := task.TriggerConfig{Cron: "0 8 * * *"}
	applyTriggerPatch(&tr, &TriggerPatch{Daemon: boolPtr(true)})
	if !tr.Daemon {
		t.Error("daemon not set")
	}
	if tr.Cron != "" {
		t.Error("cron not cleared")
	}
}

func TestApplyTriggerPatch_Restart(t *testing.T) {
	tr := task.TriggerConfig{Daemon: true}
	applyTriggerPatch(&tr, &TriggerPatch{Restart: strPtr("on-failure")})
	if tr.Restart != "on-failure" {
		t.Errorf("got %q", tr.Restart)
	}
	// Restart alone should not clear the daemon flag.
	if !tr.Daemon {
		t.Error("daemon cleared unexpectedly")
	}
}

func TestApplyTriggerPatch_NilFieldsNotApplied(t *testing.T) {
	tr := task.TriggerConfig{Cron: "0 8 * * *"}
	applyTriggerPatch(&tr, &TriggerPatch{})
	if tr.Cron != "0 8 * * *" {
		t.Error("cron changed unexpectedly")
	}
}

func TestApplyTriggerPatch_WebhookAuth(t *testing.T) {
	// Setting auth: true on a webhook override must flip WebhookAuth — this
	// is what the dicodai preset relies on to gate /hooks/ai/dicodai behind
	// the dicode session cookie.
	tr := task.TriggerConfig{Webhook: "/hooks/ai/dicodai"}
	applyTriggerPatch(&tr, &TriggerPatch{Auth: boolPtr(true)})
	if !tr.WebhookAuth {
		t.Error("WebhookAuth should be true after auth patch")
	}
	// Flipping it back off must also work.
	applyTriggerPatch(&tr, &TriggerPatch{Auth: boolPtr(false)})
	if tr.WebhookAuth {
		t.Error("WebhookAuth should be false after auth:false patch")
	}
}

func TestApplyTriggerPatch_AuthNilPreservesWebhookAuth(t *testing.T) {
	// A nil Auth pointer must not clobber an existing WebhookAuth value —
	// otherwise a webhook-only patch would silently disable auth.
	tr := task.TriggerConfig{Webhook: "/hooks/x", WebhookAuth: true}
	applyTriggerPatch(&tr, &TriggerPatch{Webhook: strPtr("/hooks/y")})
	if !tr.WebhookAuth {
		t.Error("WebhookAuth should be preserved when Auth is nil")
	}
}

// TestApplyTriggerPatch_TypeSwitchClearsAuth ensures switching away from a
// webhook trigger clears WebhookAuth alongside Webhook itself. Otherwise a
// stale `auth: true` would silently reappear the next time the trigger is
// switched back to a webhook in a later override layer — a footgun that
// produces "why isn't my auth working" well after the config looks clean.
func TestApplyTriggerPatch_TypeSwitchClearsAuth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch *TriggerPatch
	}{
		{"Cron", &TriggerPatch{Cron: strPtr("0 * * * *")}},
		{"Manual", &TriggerPatch{Manual: boolPtr(true)}},
		{"Chain", &TriggerPatch{Chain: &task.ChainTrigger{From: "upstream", On: "success"}}},
		{"Daemon", &TriggerPatch{Daemon: boolPtr(true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := task.TriggerConfig{Webhook: "/hooks/x", WebhookAuth: true}
			applyTriggerPatch(&tr, tc.patch)
			if tr.Webhook != "" {
				t.Errorf("%s patch should clear Webhook, got %q", tc.name, tr.Webhook)
			}
			if tr.WebhookAuth {
				t.Errorf("%s patch should clear WebhookAuth alongside Webhook", tc.name)
			}
		})
	}
}

// ── applyOverrides ────────────────────────────────────────────────────────────

func TestApplyOverrides_SingleLayer(t *testing.T) {
	base := baseSpec()
	got := applyOverrides(base, &Overrides{
		Timeout: 30 * time.Second,
		Env:     []task.EnvEntry{{Name: "LOG", Value: "info"}},
	})
	if got.Timeout != 30*time.Second {
		t.Errorf("timeout: got %v", got.Timeout)
	}
	em := envMap(got.Permissions.Env)
	if em["LOG"] != "info" {
		t.Errorf("env: %v", got.Permissions.Env)
	}
	// Base must not be mutated.
	if base.Timeout != 60*time.Second {
		t.Error("base spec mutated")
	}
}

func TestApplyOverrides_NameDescription(t *testing.T) {
	base := baseSpec()
	got := applyOverrides(base, &Overrides{
		Name:        "My Task",
		Description: "My description",
	})
	if got.Name != "My Task" {
		t.Errorf("name: %q", got.Name)
	}
	if got.Description != "My description" {
		t.Errorf("description: %q", got.Description)
	}
	if base.Name != "test" {
		t.Error("base name mutated")
	}
}

func TestApplyOverrides_LeafWins(t *testing.T) {
	base := baseSpec()
	layer1 := &Overrides{Timeout: 120 * time.Second}
	layer2 := &Overrides{Timeout: 30 * time.Second} // wins
	got := applyOverrides(base, layer1, layer2)
	if got.Timeout != 30*time.Second {
		t.Errorf("leaf should win: got %v", got.Timeout)
	}
}

func TestApplyOverrides_NilLayerSkipped(t *testing.T) {
	base := baseSpec()
	got := applyOverrides(base, nil, &Overrides{Timeout: 10 * time.Second}, nil)
	if got.Timeout != 10*time.Second {
		t.Errorf("got %v", got.Timeout)
	}
}

func TestApplyOverrides_EnvMerge(t *testing.T) {
	base := baseSpec() // APP=base, LOG=debug
	got := applyOverrides(base,
		&Overrides{Env: []task.EnvEntry{{Name: "RUNTIME_ENV", Value: "backend"}}},
		&Overrides{Env: []task.EnvEntry{{Name: "LOG", Value: "info"}}},
		&Overrides{Env: []task.EnvEntry{{Name: "LOG", Value: "warn"}, {Name: "REGION", Value: "eu"}}},
	)
	em := envMap(got.Permissions.Env)
	if em["APP"] != "base" {
		t.Errorf("APP: %q", em["APP"])
	}
	if em["LOG"] != "warn" {
		t.Errorf("LOG should be warn: %q", em["LOG"])
	}
	if em["RUNTIME_ENV"] != "backend" {
		t.Errorf("RUNTIME_ENV: %q", em["RUNTIME_ENV"])
	}
	if em["REGION"] != "eu" {
		t.Errorf("REGION: %q", em["REGION"])
	}
}

func TestApplyOverrides_Net(t *testing.T) {
	base := baseSpec()
	got := applyOverrides(base, &Overrides{Net: []string{"api.example.com"}})
	if len(got.Permissions.Net) != 1 || got.Permissions.Net[0] != "api.example.com" {
		t.Errorf("net: %v", got.Permissions.Net)
	}
}

func TestApplyOverrides_TriggerCronPatch(t *testing.T) {
	base := baseSpec() // cron: "0 8 * * *"
	got := applyOverrides(base, &Overrides{
		Trigger: &TriggerPatch{Cron: strPtr("0 3 * * *")},
	})
	if got.Trigger.Cron != "0 3 * * *" {
		t.Errorf("got %q", got.Trigger.Cron)
	}
}

func TestApplyOverrides_ParamPatch(t *testing.T) {
	base := baseSpec()
	got := applyOverrides(base, &Overrides{
		Params: []ParamOverride{{Name: "env", Default: "production"}},
	})
	for _, p := range got.Params {
		if p.Name == "env" && p.Default != "production" {
			t.Errorf("param env default: %q", p.Default)
		}
	}
}

// ── defaultsToOverrides ───────────────────────────────────────────────────────

func TestDefaultsToOverrides_Nil(t *testing.T) {
	if defaultsToOverrides(nil) != nil {
		t.Error("expected nil")
	}
}

func TestDefaultsToOverrides_Fields(t *testing.T) {
	d := &Defaults{
		Timeout: 90 * time.Second,
		Env:     []task.EnvEntry{{Name: "X", Value: "1"}},
		Retry:   &RetryConfig{Attempts: 3},
	}
	o := defaultsToOverrides(d)
	if o == nil {
		t.Fatal("nil")
	}
	if o.Timeout != 90*time.Second {
		t.Errorf("timeout: %v", o.Timeout)
	}
	if len(o.Env) != 1 || o.Env[0].Name != "X" || o.Env[0].Value != "1" {
		t.Errorf("env: %v", o.Env)
	}
	if o.Retry == nil || o.Retry.Attempts != 3 {
		t.Error("retry wrong")
	}
	if o.Enabled != nil {
		t.Error("enabled should not be set from defaults")
	}
}

// ── copySpec ──────────────────────────────────────────────────────────────────

func TestCopySpec_Independence(t *testing.T) {
	orig := baseSpec()
	cp := copySpec(orig)
	cp.Permissions.Env[0] = task.EnvEntry{Name: "APP", Value: "modified"}
	cp.Params[0].Default = "modified"
	if orig.Permissions.Env[0].Value != "base" {
		t.Error("original env mutated")
	}
	if orig.Params[0].Default != "staging" {
		t.Error("original param mutated")
	}
}
