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

func TestTriggerBefore_Parses(t *testing.T) {
	src := strings.TrimSpace(`
name: tunnel
runtime: docker
trigger:
  daemon: true
  before: [render-config, fetch-creds]
docker:
  image: x
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(s.Trigger.Before) != 2 || s.Trigger.Before[0].Task != "render-config" || s.Trigger.Before[1].Task != "fetch-creds" {
		t.Errorf("Before = %v", s.Trigger.Before)
	}
}

// TestTriggerBefore_PerEdgeOverrides verifies the dual-form decoder on the
// before: list: bare-ID strings, mapping forms with overrides, and a mix
// of both must all parse correctly. The override blob itself is preserved
// so the engine can apply it at preflight dispatch time.
func TestTriggerBefore_PerEdgeOverrides(t *testing.T) {
	src := strings.TrimSpace(`
name: tunnel
runtime: docker
trigger:
  daemon: true
  before:
    - render-config
    - task: fetch-creds
      overrides:
        timeout: 5m
        env:
          - name: MODE
            value: preflight
    - task: warm-cache
docker:
  image: x
`)
	var s Spec
	if err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(s.Trigger.Before) != 3 {
		t.Fatalf("Before length = %d, want 3", len(s.Trigger.Before))
	}

	// Entry 0: bare string form, no overrides.
	if s.Trigger.Before[0].Task != "render-config" {
		t.Errorf("entry[0].Task = %q, want render-config", s.Trigger.Before[0].Task)
	}
	if s.Trigger.Before[0].Overrides != nil {
		t.Errorf("entry[0].Overrides should be nil for bare-ID form, got %+v", s.Trigger.Before[0].Overrides)
	}

	// Entry 1: mapping form WITH overrides.
	if s.Trigger.Before[1].Task != "fetch-creds" {
		t.Errorf("entry[1].Task = %q, want fetch-creds", s.Trigger.Before[1].Task)
	}
	if s.Trigger.Before[1].Overrides == nil {
		t.Fatal("entry[1].Overrides should be populated")
	}
	if got := s.Trigger.Before[1].Overrides.Timeout; got != 5*time.Minute {
		t.Errorf("entry[1].Overrides.Timeout = %v, want 5m", got)
	}
	if len(s.Trigger.Before[1].Overrides.Env) != 1 ||
		s.Trigger.Before[1].Overrides.Env[0].Name != "MODE" ||
		s.Trigger.Before[1].Overrides.Env[0].Value != "preflight" {
		t.Errorf("entry[1].Overrides.Env = %+v, want one MODE=preflight entry", s.Trigger.Before[1].Overrides.Env)
	}

	// Entry 2: mapping form WITHOUT overrides — still legal.
	if s.Trigger.Before[2].Task != "warm-cache" {
		t.Errorf("entry[2].Task = %q, want warm-cache", s.Trigger.Before[2].Task)
	}
	if s.Trigger.Before[2].Overrides != nil {
		t.Errorf("entry[2].Overrides should be nil (mapping without overrides:), got %+v", s.Trigger.Before[2].Overrides)
	}
}

// TestTriggerBefore_BadEntryShapeRejected pins the negative path: a
// sequence-of-sequences or any other non-scalar / non-mapping node must
// surface an explicit decode error rather than silently dropping the
// entry. Catches the failure mode where a stray YAML structure (e.g. a
// typo'd indentation) gets parsed as a no-op.
func TestTriggerBefore_BadEntryShapeRejected(t *testing.T) {
	src := strings.TrimSpace(`
name: t
runtime: docker
trigger:
  daemon: true
  before:
    - [oops, not, scalar]
docker: { image: x }`)
	var s Spec
	err := yaml.NewDecoder(strings.NewReader(src)).Decode(&s)
	if err == nil {
		t.Fatalf("expected decode error for nested sequence in before:, got none (got Before=%+v)", s.Trigger.Before)
	}
	if !strings.Contains(err.Error(), "trigger.before entry") {
		t.Errorf("expected error to mention trigger.before entry, got: %v", err)
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

func TestTriggerBefore_Validation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "before without daemon",
			yaml: `name: t
runtime: deno
trigger: { manual: true, before: [x] }`,
			wantErr: "before: requires daemon: true",
		},
		{
			name: "self-reference",
			yaml: `name: selfref
runtime: docker
trigger: { daemon: true, before: [selfref] }
docker: { image: x }`,
			wantErr: "before: cannot reference self",
		},
		{
			name: "empty task ID",
			yaml: `name: t
runtime: docker
trigger: { daemon: true, before: [""] }
docker: { image: x }`,
			wantErr: "before: empty task ID",
		},
		{
			name: "valid empty before",
			yaml: `name: ok
runtime: docker
trigger: { daemon: true, before: [] }
docker: { image: x }`,
			wantErr: "",
		},
		{
			name: "valid populated before",
			yaml: `name: ok2
runtime: docker
trigger: { daemon: true, before: [render-config] }
docker: { image: x }`,
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Spec
			if err := yaml.NewDecoder(strings.NewReader(strings.TrimSpace(tc.yaml))).Decode(&s); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// Mimic LoadDirWithVars' post-decode initialisation: the ID is set
			// from the directory base name. The self-reference test requires
			// it to match the YAML name.
			s.ID = s.Name
			err := s.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// TestPerEdgeOverrides_RejectUnsupportedFields pins the validation that runs
// inside Spec.validate for every per-edge override site (Trigger.Before[].Overrides
// and Trigger.Chain.Overrides). Fields that don't make sense at a per-edge
// level — Enabled, Retry, Defaults, Entries, Name, Description, Trigger —
// must be rejected with an error that names the offending field so operators
// can find it in their task.yaml. See PR #303 HIGH and MED #3 review threads.
func TestPerEdgeOverrides_RejectUnsupportedFields(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantErr  string // substring that must appear in the error
		wantSite string // substring naming the site (before/chain.overrides)
	}{
		// Trigger.Before[].Overrides — HIGH finding.
		{
			name: "before edge: enabled rejected",
			yaml: `name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
      overrides:
        enabled: false`,
			wantErr:  "enabled",
			wantSite: "trigger.before",
		},
		{
			name: "before edge: retry rejected",
			yaml: `name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
      overrides:
        retry:
          attempts: 3`,
			wantErr:  "retry",
			wantSite: "trigger.before",
		},
		{
			name: "before edge: trigger rejected (MED #3)",
			yaml: `name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
      overrides:
        trigger:
          manual: true`,
			wantErr:  "trigger",
			wantSite: "trigger.before",
		},
		{
			name: "before edge: defaults rejected",
			yaml: `name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
      overrides:
        defaults:
          timeout: 5m`,
			wantErr:  "defaults",
			wantSite: "trigger.before",
		},
		{
			name: "before edge: entries rejected",
			yaml: `name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
      overrides:
        entries:
          foo: {}`,
			wantErr:  "entries",
			wantSite: "trigger.before",
		},
		{
			name: "before edge: name rejected",
			yaml: `name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
      overrides:
        name: renamed`,
			wantErr:  "name",
			wantSite: "trigger.before",
		},
		{
			name: "before edge: description rejected",
			yaml: `name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
      overrides:
        description: nope`,
			wantErr:  "description",
			wantSite: "trigger.before",
		},
		{
			name: "before edge: reserved param key rejected (MED #4)",
			yaml: `name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
      overrides:
        params:
          taskID: x`,
			wantErr:  "taskID",
			wantSite: "trigger.before",
		},

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
// reject the per-edge override fields that legitimately apply: Params (without
// reserved keys), Env, Net, FS, Timeout, Notify, Dicode, Runtime. Guards
// against an over-aggressive validator regression.
func TestPerEdgeOverrides_LegitimateFieldsAllowed(t *testing.T) {
	src := strings.TrimSpace(`
name: d
runtime: docker
docker: { image: x }
trigger:
  daemon: true
  before:
    - task: render
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
        notify:
          on_failure: true
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

// TestLoadDir_BuildinTasksParse walks every on-disk `tasks/buildin/*/task.yaml`
// (and nested provider tasks like secret-providers/doppler/) and asserts that
// LoadDir succeeds for each. This locks in the cleanup from #317 — a future
// regression that reintroduces the legacy `notify:` block (or any other field
// the strict validator rejects) surfaces immediately rather than waiting for a
// downstream test to exercise the load path.
func TestLoadDir_BuildinTasksParse(t *testing.T) {
	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot anchor tasks/buildin path")
	}
	// thisFile is .../pkg/task/spec_test.go
	// → repo root is two levels up from the file's directory.
	pkgDir := filepath.Dir(thisFile)               // .../pkg/task
	repoRoot := filepath.Dir(filepath.Dir(pkgDir)) // .../
	buildinRoot := filepath.Join(repoRoot, "tasks", "buildin")
	if _, err := os.Stat(buildinRoot); err != nil {
		t.Fatalf("tasks/buildin not found at %s: %v", buildinRoot, err)
	}

	var taskDirs []string
	err := filepath.Walk(buildinRoot, func(path string, info os.FileInfo, err error) error {
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
		t.Fatalf("walk %s: %v", buildinRoot, err)
	}
	if len(taskDirs) == 0 {
		t.Fatalf("no task.yaml files found under %s", buildinRoot)
	}

	for _, dir := range taskDirs {
		rel, _ := filepath.Rel(repoRoot, dir)
		t.Run(rel, func(t *testing.T) {
			// Provide TASK_SET_DIR so tasks that reference ${TASK_SET_DIR}
			// (e.g. ai-agent-claude-cli) resolve to a concrete path. The
			// buildin source's effective TASK_SET_DIR is the tasks/buildin
			// directory itself (where the taskset.yaml lives).
			extras := map[string]string{
				VarTaskSetDir: buildinRoot,
			}
			if _, err := LoadDirWithVars(dir, extras); err != nil {
				t.Fatalf("LoadDirWithVars(%s): %v", dir, err)
			}
		})
	}
}
