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
