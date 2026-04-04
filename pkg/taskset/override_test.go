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
		Params: []task.Param{
			{Name: "env", Default: "staging"},
			{Name: "region", Default: "us-east-1"},
		},
	}
}

// envMap converts permissions.env entries to a name→value map for assertions.
func envMap(entries []task.EnvEntry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.Name] = e.Value
	}
	return m
}

// ── mergeEnv ──────────────────────────────────────────────────────────────────

func TestMergeEnv_NewKeyAppended(t *testing.T) {
	got := mergeEnv([]string{"A=1"}, []string{"B=2"})
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %v", got)
	}
	if got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("unexpected: %v", got)
	}
}

func TestMergeEnv_OverlayWins(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, []string{"A=99"})
	want := map[string]string{"A": "A=99", "B": "B=2"}
	for _, e := range got {
		k := envKey(e)
		if want[k] != e {
			t.Errorf("key %s: got %q, want %q", k, e, want[k])
		}
	}
}

func TestMergeEnv_PreservesOrder(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2", "C=3"}, []string{"B=99", "D=4"})
	// A, B, C from base order; D appended
	if len(got) != 4 {
		t.Fatalf("want 4, got %v", got)
	}
	keys := make([]string, len(got))
	for i, e := range got {
		keys[i] = envKey(e)
	}
	if keys[0] != "A" || keys[1] != "B" || keys[2] != "C" || keys[3] != "D" {
		t.Errorf("order wrong: %v", keys)
	}
}

func TestMergeEnv_BareKey(t *testing.T) {
	// Bare key (no '=') is treated as a key reference, merged by name.
	got := mergeEnv([]string{"SECRET"}, []string{"SECRET=explicit"})
	if len(got) != 1 || got[0] != "SECRET=explicit" {
		t.Errorf("unexpected: %v", got)
	}
}

// ── mergeParams ───────────────────────────────────────────────────────────────

func TestMergeParams_PatchExisting(t *testing.T) {
	params := []task.Param{{Name: "env", Default: "staging"}}
	mergeParams(&params, []ParamOverride{{Name: "env", Default: "production"}})
	if params[0].Default != "production" {
		t.Errorf("got %q", params[0].Default)
	}
}

func TestMergeParams_AppendNew(t *testing.T) {
	params := []task.Param{{Name: "env", Default: "staging"}}
	mergeParams(&params, []ParamOverride{{Name: "region", Default: "eu-west-1"}})
	if len(params) != 2 {
		t.Fatalf("want 2 params, got %d", len(params))
	}
	if params[1].Name != "region" || params[1].Default != "eu-west-1" {
		t.Errorf("wrong new param: %+v", params[1])
	}
}

func TestMergeParams_PatchRequired(t *testing.T) {
	params := []task.Param{{Name: "env", Required: false}}
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
	// Empty patch — nothing changes.
	applyTriggerPatch(&tr, &TriggerPatch{})
	if tr.Cron != "0 8 * * *" {
		t.Error("cron changed unexpectedly")
	}
}

// ── applyOverrides ────────────────────────────────────────────────────────────

func TestApplyOverrides_SingleLayer(t *testing.T) {
	base := baseSpec()
	got := applyOverrides(base, &Overrides{
		Timeout: 30 * time.Second,
		Env:     []string{"LOG=info"},
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
	// Config (level 2): adds RUNTIME_ENV
	// Set defaults (level 3): sets LOG
	// Entry (level 6): overrides LOG, adds REGION
	base := baseSpec() // APP=base, LOG=debug
	got := applyOverrides(base,
		&Overrides{Env: []string{"RUNTIME_ENV=backend"}},
		&Overrides{Env: []string{"LOG=info"}},
		&Overrides{Env: []string{"LOG=warn", "REGION=eu"}},
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
		Env:     []string{"X=1"},
		Retry:   &RetryConfig{Attempts: 3},
	}
	o := defaultsToOverrides(d)
	if o == nil {
		t.Fatal("nil")
	}
	if o.Timeout != 90*time.Second {
		t.Errorf("timeout: %v", o.Timeout)
	}
	if len(o.Env) != 1 || o.Env[0] != "X=1" {
		t.Errorf("env: %v", o.Env)
	}
	if o.Retry == nil || o.Retry.Attempts != 3 {
		t.Error("retry wrong")
	}
	// trigger/params/enabled must NOT be carried over by defaultsToOverrides.
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
