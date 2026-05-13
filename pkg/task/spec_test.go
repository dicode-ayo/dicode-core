package task

import (
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
	if len(s.Trigger.Before) != 2 || s.Trigger.Before[0] != "render-config" || s.Trigger.Before[1] != "fetch-creds" {
		t.Errorf("Before = %v", s.Trigger.Before)
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
