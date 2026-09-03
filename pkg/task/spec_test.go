package task

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSpec_AutoFix_Parses(t *testing.T) {
	yamlSrc := []byte(`
name: test
runtime: deno
trigger: { manual: true }
auto_fix:
  include_input: false
  show_redacted_field_names: false
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	if s.AutoFix == nil {
		t.Fatal("AutoFix not parsed")
	}
	if s.AutoFix.IncludeInput == nil || *s.AutoFix.IncludeInput != false {
		t.Errorf("IncludeInput = %v, want false ptr", s.AutoFix.IncludeInput)
	}
	if s.AutoFix.ShowRedactedFieldNames == nil || *s.AutoFix.ShowRedactedFieldNames != false {
		t.Errorf("ShowRedactedFieldNames = %v, want false ptr", s.AutoFix.ShowRedactedFieldNames)
	}
}

func TestSpec_RunInputsOverride_Parses(t *testing.T) {
	yamlSrc := []byte(`
name: test
runtime: deno
trigger: { manual: true }
run_inputs:
  enabled: false
  retention: 1h
  body_full_textual: true
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	if s.RunInputs == nil {
		t.Fatal("RunInputs not parsed")
	}
	if s.RunInputs.Enabled == nil || *s.RunInputs.Enabled != false {
		t.Errorf("Enabled wrong")
	}
	if s.RunInputs.Retention != time.Hour {
		t.Errorf("Retention = %v", s.RunInputs.Retention)
	}
	if s.RunInputs.BodyFullTextual == nil || *s.RunInputs.BodyFullTextual != true {
		t.Errorf("BodyFullTextual wrong")
	}
}

func TestSpec_RunResultOverride_Parses(t *testing.T) {
	yamlSrc := []byte(`
name: test
runtime: deno
trigger: { manual: true }
run_result:
  enabled: false
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	if s.RunResult == nil {
		t.Fatal("RunResult not parsed")
	}
	if s.RunResult.Enabled == nil || *s.RunResult.Enabled != false {
		t.Errorf("RunResult.Enabled = %v, want false ptr", s.RunResult.Enabled)
	}
}

func TestSpec_MCPExposed_Parses(t *testing.T) {
	yamlSrc := []byte(`
name: exposed-task
runtime: deno
trigger: { manual: true }
mcp_exposed: true
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	if !s.MCPExposed {
		t.Error("MCPExposed = false, want true")
	}
}

func TestSpec_MCPExposed_DefaultFalse(t *testing.T) {
	yamlSrc := []byte(`
name: internal-task
runtime: deno
trigger: { manual: true }
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	if s.MCPExposed {
		t.Error("MCPExposed = true, want false (default)")
	}
}

func TestSpec_EnvReadExposed_Parses(t *testing.T) {
	yamlSrc := []byte(`
name: exposed-env
runtime: deno
trigger: { manual: true }
permissions:
  env_read_exposed: true
  env:
    - DICODE_DATADIR
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	if !s.Permissions.EnvReadExposed {
		t.Error("EnvReadExposed = false, want true")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("env_read_exposed with named entries should validate, got %v", err)
	}
}

func TestSpec_Validate_RejectsEnvStarEntry(t *testing.T) {
	yamlSrc := []byte(`
name: legacy-wildcard
runtime: deno
trigger: { manual: true }
permissions:
  env:
    - "*"
    - DICODE_DATADIR
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("a name-only \"*\" env entry must be rejected")
	}
	if !strings.Contains(err.Error(), "env_read_exposed") {
		t.Errorf("error should point to env_read_exposed, got %v", err)
	}
}

func TestSpec_HashInclude_Parses(t *testing.T) {
	yamlSrc := []byte(`
name: test
runtime: deno
trigger: { manual: true }
hash_include:
  - "../ai-agent-core/chat.ts"
  - "../shared"
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	want := []string{"../ai-agent-core/chat.ts", "../shared"}
	if len(s.HashInclude) != len(want) {
		t.Fatalf("HashInclude = %v, want %v", s.HashInclude, want)
	}
	for i, w := range want {
		if s.HashInclude[i] != w {
			t.Errorf("HashInclude[%d] = %q, want %q", i, s.HashInclude[i], w)
		}
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("valid hash_include should validate, got %v", err)
	}
}

func TestSpec_Validate_RejectsEmptyHashIncludeEntry(t *testing.T) {
	s := Spec{
		Name:    "test",
		Runtime: RuntimeDeno,
		Trigger: TriggerConfig{Manual: true},
		HashInclude: []string{
			"",
		},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("an empty hash_include entry must be rejected")
	}
	if !strings.Contains(err.Error(), "hash_include") {
		t.Errorf("error should mention hash_include, got %v", err)
	}
}

func TestSpec_Validate_RejectsAbsoluteHashIncludeEntry(t *testing.T) {
	s := Spec{
		Name:    "test",
		Runtime: RuntimeDeno,
		Trigger: TriggerConfig{Manual: true},
		HashInclude: []string{
			"/etc/passwd",
		},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("an absolute hash_include entry must be rejected")
	}
	if !strings.Contains(err.Error(), "hash_include") {
		t.Errorf("error should mention hash_include, got %v", err)
	}
}

// TestSpec_Validate_RejectsLexicallyOutOfScopeHashInclude is the regression
// for a finding caught in review: a hash_include entry that resolves past
// the task's parent directory boundary (task.Hash's resolveInclude enforces
// this at hash time) must be rejected here too, at config-load time —
// otherwise it only fails inside task.Hash later, where
// pkg/taskset/source.go's snapHash silently falls back to a spec-only hash
// on any Hash() error, dropping ALL dir-content change detection for the
// task (not just the broken include) until the entry is fixed.
func TestSpec_Validate_RejectsLexicallyOutOfScopeHashInclude(t *testing.T) {
	cases := []string{
		"..",                   // exactly the boundary itself
		"../..",                // one hop past the boundary
		"../../outside",        // two hops up, then a path
		"sub/../../../outside", // "sub/.." cancels to the start; the remaining two ".." net two hops up
	}
	for _, inc := range cases {
		t.Run(inc, func(t *testing.T) {
			s := Spec{
				Name:        "test",
				Runtime:     RuntimeDeno,
				Trigger:     TriggerConfig{Manual: true},
				HashInclude: []string{inc},
			}
			err := s.Validate()
			if err == nil {
				t.Fatalf("hash_include %q must be rejected as lexically out of scope", inc)
			}
			if !strings.Contains(err.Error(), "hash_include") {
				t.Errorf("error should mention hash_include, got %v", err)
			}
		})
	}
}

// TestSpec_Validate_AcceptsLexicallyInScopeHashInclude is the control for
// the test above: entries that stay within the one-hop sibling-task
// boundary (or don't escape dir at all) must still validate.
func TestSpec_Validate_AcceptsLexicallyInScopeHashInclude(t *testing.T) {
	cases := []string{
		"../sibling-task/file.ts", // one hop up, then down — the feature's actual use case
		"../sibling.ts",
		"foo.ts",     // stays inside dir itself — redundant but harmless
		"lib/foo.ts", // same, nested
	}
	for _, inc := range cases {
		t.Run(inc, func(t *testing.T) {
			s := Spec{
				Name:        "test",
				Runtime:     RuntimeDeno,
				Trigger:     TriggerConfig{Manual: true},
				HashInclude: []string{inc},
			}
			if err := s.Validate(); err != nil {
				t.Fatalf("hash_include %q should validate, got %v", inc, err)
			}
		})
	}
}

func TestSpec_RunResultOverride_Omitted(t *testing.T) {
	yamlSrc := []byte(`
name: test
runtime: deno
trigger: { manual: true }
`)
	var s Spec
	if err := yaml.Unmarshal(yamlSrc, &s); err != nil {
		t.Fatal(err)
	}
	if s.RunResult != nil {
		t.Errorf("RunResult should be nil when omitted, got %+v", s.RunResult)
	}
}

func TestSpec_ProviderBlockRoundTrip(t *testing.T) {
	src := strings.TrimSpace(`
name: doppler
runtime: deno
trigger:
  manual: true
provider:
  cache_ttl: 5m
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Provider == nil {
		t.Fatalf("Provider was nil")
	}
	if s.Provider.CacheTTL != 5*time.Minute {
		t.Fatalf("CacheTTL = %v, want 5m", s.Provider.CacheTTL)
	}
}

// TestSpec_WebuiNav_RoundTrip verifies that a webui.nav block decodes into
// Spec.Webui.Nav.{Label,Order,Icon} (#222).
func TestSpec_WebuiNav_RoundTrip(t *testing.T) {
	src := strings.TrimSpace(`
name: auth-providers
runtime: deno
trigger:
  webhook: /hooks/auth-providers
webui:
  nav:
    label: "Auth Providers"
    order: 5
    icon: "key"
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Webui == nil || s.Webui.Nav == nil {
		t.Fatalf("Webui.Nav not parsed, got %+v", s.Webui)
	}
	if s.Webui.Nav.Label != "Auth Providers" {
		t.Errorf("Label = %q, want %q", s.Webui.Nav.Label, "Auth Providers")
	}
	if s.Webui.Nav.Order != 5 {
		t.Errorf("Order = %d, want 5", s.Webui.Nav.Order)
	}
	if s.Webui.Nav.Icon != "key" {
		t.Errorf("Icon = %q, want %q", s.Webui.Nav.Icon, "key")
	}
	if err := s.validate(); err != nil {
		t.Fatalf("valid webui.nav should validate, got %v", err)
	}
}

// TestSpec_Validate_RejectsEmptyWebuiNavLabel ensures a webui.nav block with
// an empty label is rejected at config-load time with a clear error (#222).
func TestSpec_Validate_RejectsEmptyWebuiNavLabel(t *testing.T) {
	src := strings.TrimSpace(`
name: bad-nav
runtime: deno
trigger: { manual: true }
webui:
  nav:
    order: 1
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	err := s.validate()
	if err == nil {
		t.Fatal("a webui.nav block with empty label must be rejected")
	}
	if !strings.Contains(err.Error(), "webui.nav.label") {
		t.Errorf("error should mention webui.nav.label, got %v", err)
	}
}

// TestSpec_WebuiNav_Omitted verifies a spec with no webui key at all still
// validates fine (nil-safe) — the common case for tasks not opting into nav
// contribution (#222).
func TestSpec_WebuiNav_Omitted(t *testing.T) {
	src := strings.TrimSpace(`
name: plain-task
runtime: deno
trigger: { manual: true }
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Webui != nil {
		t.Errorf("Webui should be nil when omitted, got %+v", s.Webui)
	}
	if err := s.validate(); err != nil {
		t.Fatalf("spec with no webui key should validate, got %v", err)
	}
}

func TestDockerConfig_HardeningFields_Parse(t *testing.T) {
	src := strings.TrimSpace(`
name: tunnel
runtime: docker
trigger:
  daemon: true
docker:
  image: cloudflare/cloudflared:latest
  network_mode: bridge
  extra_hosts:
    - host.docker.internal:host-gateway
    - api.local:10.0.0.5
  cap_drop: [ALL]
  security_opt:
    - "no-new-privileges:true"
  read_only: true
  user: "65532:65532"
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Docker == nil {
		t.Fatalf("Docker was nil")
	}
	d := s.Docker
	if d.NetworkMode != "bridge" {
		t.Errorf("NetworkMode = %q, want bridge", d.NetworkMode)
	}
	if got, want := d.ExtraHosts, []string{"host.docker.internal:host-gateway", "api.local:10.0.0.5"}; !equalSlice(got, want) {
		t.Errorf("ExtraHosts = %v, want %v", got, want)
	}
	if got, want := d.CapDrop, []string{"ALL"}; !equalSlice(got, want) {
		t.Errorf("CapDrop = %v, want %v", got, want)
	}
	if got, want := d.SecurityOpt, []string{"no-new-privileges:true"}; !equalSlice(got, want) {
		t.Errorf("SecurityOpt = %v, want %v", got, want)
	}
	if !d.ReadOnly {
		t.Errorf("ReadOnly = false, want true")
	}
	if d.User != "65532:65532" {
		t.Errorf("User = %q, want 65532:65532", d.User)
	}
}

func TestDockerConfig_HardeningFields_DefaultsZero(t *testing.T) {
	src := strings.TrimSpace(`
name: minimal
runtime: docker
trigger: { manual: true }
docker:
  image: alpine
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := s.Docker
	if d.NetworkMode != "" || len(d.ExtraHosts) != 0 || len(d.CapDrop) != 0 ||
		len(d.SecurityOpt) != 0 || d.ReadOnly || d.User != "" {
		t.Errorf("hardening fields should be zero-valued when omitted, got %+v", d)
	}
}

func TestDockerConfig_HardeningWarnings(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		mustMatch []string // substrings that must appear in s.Warnings
	}{
		{
			name: "network_mode host warns",
			yaml: `
name: t
runtime: docker
trigger: { manual: true }
docker:
  image: alpine
  network_mode: host
`,
			mustMatch: []string{"network_mode: host"},
		},
		{
			name: "cap_add SYS_ADMIN warns",
			yaml: `
name: t
runtime: docker
trigger: { manual: true }
docker:
  image: alpine
  cap_add: [SYS_ADMIN]
`,
			mustMatch: []string{"SYS_ADMIN"},
		},
		{
			name: "seccomp unconfined warns",
			yaml: `
name: t
runtime: docker
trigger: { manual: true }
docker:
  image: alpine
  security_opt: ["seccomp=unconfined"]
`,
			mustMatch: []string{"seccomp=unconfined"},
		},
		{
			name: "hardened config has no warnings",
			yaml: `
name: t
runtime: docker
trigger: { manual: true }
docker:
  image: alpine
  network_mode: bridge
  cap_drop: [ALL]
  security_opt: ["no-new-privileges:true"]
  read_only: true
`,
			mustMatch: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Spec
			if err := yaml.NewDecoder(strings.NewReader(strings.TrimSpace(tc.yaml))).Decode(&s); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := s.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			joined := strings.Join(s.Warnings, "|")
			for _, want := range tc.mustMatch {
				if !strings.Contains(joined, want) {
					t.Errorf("missing warning containing %q; got %v", want, s.Warnings)
				}
			}
			if len(tc.mustMatch) == 0 && len(s.Warnings) != 0 {
				t.Errorf("expected no warnings, got %v", s.Warnings)
			}
		})
	}
}

func TestWebhookSecretGatedFields_Warnings(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		mustMatch []string // substrings that must appear in s.Warnings
	}{
		{
			name: "require_timestamp without webhook_secret warns",
			yaml: `
name: t
trigger:
  webhook: /hooks/t
  require_timestamp: true
`,
			mustMatch: []string{"require_timestamp", "webhook_secret is empty"},
		},
		{
			name: "replay_protection true without webhook_secret warns",
			yaml: `
name: t
trigger:
  webhook: /hooks/t
  replay_protection: true
`,
			mustMatch: []string{"replay_protection", "webhook_secret is empty"},
		},
		{
			name: "require_timestamp false without webhook_secret does not warn",
			yaml: `
name: t
trigger:
  webhook: /hooks/t
  require_timestamp: false
`,
			mustMatch: nil,
		},
		{
			// false matches the no-op default when there's no secret to
			// protect either way — a legitimate, self-consistent config, not
			// a misconfiguration worth warning about.
			name: "replay_protection false without webhook_secret does not warn",
			yaml: `
name: t
trigger:
  webhook: /hooks/t
  replay_protection: false
`,
			mustMatch: nil,
		},
		{
			name: "require_timestamp with webhook_secret has no warning",
			yaml: `
name: t
trigger:
  webhook: /hooks/t
  webhook_secret: "shh"
  require_timestamp: true
  replay_protection: true
`,
			mustMatch: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Spec
			if err := yaml.NewDecoder(strings.NewReader(strings.TrimSpace(tc.yaml))).Decode(&s); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := s.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			joined := strings.Join(s.Warnings, "|")
			for _, want := range tc.mustMatch {
				if !strings.Contains(joined, want) {
					t.Errorf("missing warning containing %q; got %v", want, s.Warnings)
				}
			}
			if len(tc.mustMatch) == 0 && len(s.Warnings) != 0 {
				t.Errorf("expected no warnings, got %v", s.Warnings)
			}
		})
	}
}

func TestChainTrigger_Params_Parses(t *testing.T) {
	src := strings.TrimSpace(`
name: downstream
runtime: deno
trigger:
  chain:
    from: upstream
    on: success
    params:
      mode: production
      shard: "3"
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Trigger.Chain == nil {
		t.Fatal("Chain nil")
	}
	if got := s.Trigger.Chain.Params["mode"]; got != "production" {
		t.Errorf("Params[mode] = %v, want production", got)
	}
	if got := s.Trigger.Chain.Params["shard"]; got != "3" {
		t.Errorf("Params[shard] = %v, want 3", got)
	}
}

func TestChainTrigger_Params_ReservedKeyRejected(t *testing.T) {
	for _, reserved := range []string{"taskID", "runID", "status", "output", "_chain_depth"} {
		t.Run(reserved, func(t *testing.T) {
			src := "name: t\nruntime: deno\ntrigger:\n  chain:\n    from: x\n    params:\n      " + reserved + ": v\n"
			var s Spec
			if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := s.validate(); err == nil || !strings.Contains(err.Error(), reserved) {
				t.Errorf("expected error mentioning %q, got: %v", reserved, err)
			}
		})
	}
}

// TestTriggerChain_Overrides_Parses verifies that trigger.chain.overrides
// decodes alongside the existing chain fields. The override blob is held
// as a *task.Overrides pointer; nil means "no per-edge override" (the
// pre-existing behaviour preserved for backwards compat).
func TestTriggerChain_Overrides_Parses(t *testing.T) {
	src := strings.TrimSpace(`
name: downstream
runtime: deno
trigger:
  chain:
    from: upstream
    on: success
    overrides:
      params:
        mode: prod
      timeout: 90s
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Trigger.Chain == nil {
		t.Fatal("Chain not parsed")
	}
	if s.Trigger.Chain.Overrides == nil {
		t.Fatal("Chain.Overrides not parsed")
	}
	if s.Trigger.Chain.Overrides.Timeout != 90*time.Second {
		t.Errorf("Chain.Overrides.Timeout = %v, want 90s", s.Trigger.Chain.Overrides.Timeout)
	}
	if len(s.Trigger.Chain.Overrides.Params) != 1 ||
		s.Trigger.Chain.Overrides.Params[0].Name != "mode" ||
		s.Trigger.Chain.Overrides.Params[0].Default != "prod" {
		t.Errorf("Chain.Overrides.Params = %+v, want one mode=prod entry", s.Trigger.Chain.Overrides.Params)
	}
}

// TestTriggerChain_NoOverrides_BackwardsCompat verifies that omitting
// trigger.chain.overrides leaves the field nil, preserving the existing
// dispatch path for task.yamls written before the per-edge extension.
func TestTriggerChain_NoOverrides_BackwardsCompat(t *testing.T) {
	src := strings.TrimSpace(`
name: downstream
runtime: deno
trigger:
  chain:
    from: upstream
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Trigger.Chain == nil {
		t.Fatal("Chain not parsed")
	}
	if s.Trigger.Chain.Overrides != nil {
		t.Errorf("Chain.Overrides should be nil when absent, got %+v", s.Trigger.Chain.Overrides)
	}
}

// TestPerEdgeOverrides_RejectUnsupportedFields pins the validation that runs
// inside Spec.validate for the per-edge override site (Trigger.Chain.Overrides).
// Fields that don't make sense at a per-edge level — Enabled, Retry, Defaults,
// Entries, Name, Description, Trigger — must be rejected with an error that
// names the offending field so operators can find it in their task.yaml.
func TestPerEdgeOverrides_RejectUnsupportedFields(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantErr  string // substring that must appear in the error
		wantSite string // substring naming the site (before/chain.overrides)
	}{
		// Trigger.Chain.Overrides — same rules apply.
		{
			name: "chain edge: enabled rejected",
			yaml: `name: d
runtime: deno
trigger:
  chain:
    from: upstream
    overrides:
      enabled: false`,
			wantErr:  "enabled",
			wantSite: "trigger.chain.overrides",
		},
		{
			name: "chain edge: retry rejected",
			yaml: `name: d
runtime: deno
trigger:
  chain:
    from: upstream
    overrides:
      retry:
        attempts: 3`,
			wantErr:  "retry",
			wantSite: "trigger.chain.overrides",
		},
		{
			name: "chain edge: trigger rejected (MED #3)",
			yaml: `name: d
runtime: deno
trigger:
  chain:
    from: upstream
    overrides:
      trigger:
        manual: true`,
			wantErr:  "trigger",
			wantSite: "trigger.chain.overrides",
		},
		{
			name: "chain edge: reserved param key rejected (MED #4)",
			yaml: `name: d
runtime: deno
trigger:
  chain:
    from: upstream
    overrides:
      params:
        runID: x`,
			wantErr:  "runID",
			wantSite: "trigger.chain.overrides",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Spec
			if err := yaml.NewDecoder(strings.NewReader(strings.TrimSpace(tc.yaml))).Decode(&s); err != nil {
				t.Fatalf("decode: %v", err)
			}
			err := s.validate()
			if err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention offending field %q", err.Error(), tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantSite) {
				t.Errorf("error %q does not name the site %q", err.Error(), tc.wantSite)
			}
		})
	}
}

// TestPerEdgeOverrides_LegitimateFieldsAllowed verifies the validator does NOT
// reject the per-edge override fields that legitimately apply on a chain edge:
// Params (without reserved keys), Env, Net, FS, Timeout, Runtime. Guards
// against an over-aggressive validatePerEdgeOverrides regression. (chain is
// the per-edge override site validated by Spec.validate.)
func TestPerEdgeOverrides_LegitimateFieldsAllowed(t *testing.T) {
	src := strings.TrimSpace(`
name: d
runtime: docker
docker: { image: x }
trigger:
  chain:
    from: upstream
    overrides:
      params:
        mode: prod
      env:
        - name: FOO
          value: bar
      net: ["api.example.com"]
      fs:
        - path: /tmp
          permission: rw
      timeout: 30s
      runtime: deno
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := s.validate(); err != nil {
		t.Fatalf("expected validate() to accept legitimate per-edge fields, got: %v", err)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLoadDir_RejectsLegacyNotify ensures the removed per-task `notify:`
// field is rejected at load time. yaml.v3 would otherwise drop it silently
// and the task author would lose alerts they think are still configured
// (#279).
func TestLoadDir_RejectsLegacyNotify(t *testing.T) {
	dir := t.TempDir()
	src := strings.TrimSpace(`
name: legacy-notify-task
runtime: deno
trigger: { manual: true }
notify:
  on_success: false
  on_failure: true
`) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.ts"), []byte("export default async function main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadDirWithVars(dir, nil)
	if err == nil {
		t.Fatal("LoadDirWithVars accepted legacy notify block; want error")
	}
	if !strings.Contains(err.Error(), "on_failure_chain") {
		t.Errorf("error = %v; want mention of on_failure_chain migration", err)
	}
}

// TestLoadDir_ZeroTimeoutPreserved verifies that omitting timeout: in task.yaml
// leaves spec.Timeout == 0 (no deadline) for both Deno and Python runtimes.
// The old code coerced zero to 60s for non-container/non-daemon tasks, which
// contradicted the "no built-in default timeout" contract.
func TestLoadDir_ZeroTimeoutPreserved(t *testing.T) {
	for _, rt := range []string{"deno", "python"} {
		t.Run(rt, func(t *testing.T) {
			dir := t.TempDir()
			src := "name: timeout-test\nruntime: " + rt + "\ntrigger: { manual: true }\n"
			if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			spec, err := LoadDirWithVars(dir, nil)
			if err != nil {
				t.Fatalf("LoadDirWithVars: %v", err)
			}
			if spec.Timeout != 0 {
				t.Errorf("runtime=%s: spec.Timeout = %v, want 0 (no deadline)", rt, spec.Timeout)
			}
		})
	}
}

// TestLoadDir_OpsTasksParse walks every on-disk `tasks/ops/*/task.yaml` and
// asserts LoadKindedDir succeeds — the same reconciler path that runs the
// strict validator. Ops tasks ship with no Deno test that exercises the Go
// loader, so without this a spec the reconciler would reject (e.g. two trigger
// types on one task) can pass CI and never register at runtime.
func TestLoadDir_OpsTasksParse(t *testing.T) {
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot anchor tasks/ops path")
	}
	pkgDir := filepath.Dir(thisFile)               // .../pkg/task
	repoRoot := filepath.Dir(filepath.Dir(pkgDir)) // .../
	opsRoot := filepath.Join(repoRoot, "tasks", "ops")
	if _, err := os.Stat(opsRoot); err != nil {
		t.Skipf("tasks/ops not found at %s: %v", opsRoot, err)
	}

	var taskDirs []string
	err := filepath.Walk(opsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Base(path) != "task.yaml" {
			return nil
		}
		taskDirs = append(taskDirs, filepath.Dir(path))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", opsRoot, err)
	}
	if len(taskDirs) == 0 {
		t.Fatalf("no task.yaml files found under %s", opsRoot)
	}

	for _, dir := range taskDirs {
		rel, _ := filepath.Rel(repoRoot, dir)
		t.Run(rel, func(t *testing.T) {
			extras := map[string]string{VarTaskSetDir: opsRoot}
			if _, err := LoadKindedDir(dir, extras); err != nil {
				t.Fatalf("LoadKindedDir(%s): %v", dir, err)
			}
		})
	}
}

// TestScriptPath_SymlinkRejected verifies that ScriptPath returns empty string
// when task.py (or any other script) is a symlink, preventing the daemon from
// reading files outside the task directory via symlink traversal.
func TestScriptPath_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.py")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "task.py")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: t\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Spec{TaskDir: dir, Runtime: "python"}
	if got := s.ScriptPath(); got != "" {
		t.Errorf("ScriptPath() = %q, want empty (symlink should be rejected)", got)
	}
}

// TestScriptPath_SymlinkRejected_Deno mirrors the same test for the Deno runtime.
func TestScriptPath_SymlinkRejected_Deno(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.js")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "task.js")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: t\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Spec{TaskDir: dir, Runtime: RuntimeDeno}
	if got := s.ScriptPath(); got != "" {
		t.Errorf("ScriptPath() = %q, want empty (symlink should be rejected)", got)
	}
}
