package task

import (
	"testing"
)

func TestValidateParams_AcceptsCoercedTypes(t *testing.T) {
	declared := Params{
		{Name: "repo", Type: "string"},
		{Name: "limit", Type: "number"},
		{Name: "verbose", Type: "boolean"},
		{Name: "cadence", Type: "cron"},
	}
	in := map[string]any{
		"repo":    "deno/deno",
		"limit":   float64(10), // JSON numbers decode to float64
		"verbose": true,
		"cadence": "0 9 * * *",
	}
	out, errs := ValidateParams(declared, in)
	if errs != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if out["repo"] != "deno/deno" {
		t.Errorf("repo: got %q", out["repo"])
	}
	if out["limit"] != "10" {
		t.Errorf("limit: got %q", out["limit"])
	}
	if out["verbose"] != "true" {
		t.Errorf("verbose: got %q", out["verbose"])
	}
	if out["cadence"] != "0 9 * * *" {
		t.Errorf("cadence: got %q", out["cadence"])
	}
}

func TestValidateParams_FillsDefaults(t *testing.T) {
	declared := Params{
		{Name: "repo", Type: "string", Default: "deno/deno"},
		{Name: "limit", Type: "number", Default: "5"},
	}
	out, errs := ValidateParams(declared, map[string]any{})
	if errs != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if out["repo"] != "deno/deno" || out["limit"] != "5" {
		t.Errorf("defaults not applied: %v", out)
	}
}

func TestValidateParams_RequiredMissing(t *testing.T) {
	declared := Params{
		{Name: "repo", Type: "string", Required: true},
	}
	_, errs := ValidateParams(declared, map[string]any{})
	if len(errs) != 1 || errs[0].Field != "repo" || errs[0].Message != "required" {
		t.Errorf("expected single 'required' error on repo, got %v", errs)
	}
}

func TestValidateParams_RequiredWithDefaultIsSatisfied(t *testing.T) {
	declared := Params{
		{Name: "repo", Type: "string", Required: true, Default: "deno/deno"},
	}
	out, errs := ValidateParams(declared, map[string]any{})
	if errs != nil {
		t.Fatalf("default should satisfy required: %v", errs)
	}
	if out["repo"] != "deno/deno" {
		t.Errorf("got %q", out["repo"])
	}
}

func TestValidateParams_TypeMismatchNumber(t *testing.T) {
	declared := Params{{Name: "limit", Type: "number"}}
	_, errs := ValidateParams(declared, map[string]any{"limit": "not-a-number"})
	if len(errs) != 1 || errs[0].Field != "limit" {
		t.Errorf("expected type-mismatch on limit, got %v", errs)
	}
}

func TestValidateParams_TypeMismatchBoolean(t *testing.T) {
	declared := Params{{Name: "verbose", Type: "boolean"}}
	_, errs := ValidateParams(declared, map[string]any{"verbose": "yes"})
	if len(errs) != 1 || errs[0].Field != "verbose" {
		t.Errorf("expected type-mismatch on verbose, got %v", errs)
	}
}

func TestValidateParams_UnknownKeyRejected(t *testing.T) {
	declared := Params{{Name: "repo", Type: "string"}}
	_, errs := ValidateParams(declared, map[string]any{
		"repo":  "deno/deno",
		"extra": "surprise",
	})
	if len(errs) != 1 || errs[0].Field != "extra" || errs[0].Message != "unknown parameter" {
		t.Errorf("expected unknown-param on 'extra', got %v", errs)
	}
}

func TestValidateParams_AggregatesMultipleErrors(t *testing.T) {
	declared := Params{
		{Name: "repo", Type: "string", Required: true},
		{Name: "limit", Type: "number"},
	}
	_, errs := ValidateParams(declared, map[string]any{
		"limit": "not-a-number",
		"junk":  1,
	})
	if len(errs) != 3 {
		t.Errorf("expected 3 errors (required, type, unknown), got %d: %v", len(errs), errs)
	}
}

func TestValidateParams_StringAcceptsNumericInput(t *testing.T) {
	declared := Params{{Name: "label", Type: "string"}}
	out, errs := ValidateParams(declared, map[string]any{"label": float64(42)})
	if errs != nil {
		t.Fatalf("string should accept stringifiable scalars: %v", errs)
	}
	if out["label"] != "42" {
		t.Errorf("got %q", out["label"])
	}
}

func TestValidateParams_CronEmptyRejected(t *testing.T) {
	declared := Params{{Name: "schedule", Type: "cron"}}
	_, errs := ValidateParams(declared, map[string]any{"schedule": ""})
	if len(errs) != 1 {
		t.Errorf("expected error for empty cron, got %v", errs)
	}
}

func TestParamErrors_Error(t *testing.T) {
	if (ParamErrors{}).Error() == "" {
		t.Error("empty ParamErrors should still produce a message")
	}
	one := ParamErrors{{Field: "repo", Message: "required"}}
	if one.Error() == "" {
		t.Error("single-field ParamErrors should produce a message")
	}
	multi := ParamErrors{
		{Field: "a", Message: "x"},
		{Field: "b", Message: "y"},
	}
	if multi.Error() == "" {
		t.Error("multi-field ParamErrors should produce a message")
	}
}

func TestValidateParams_RequiredRejectsEmptyValue(t *testing.T) {
	declared := Params{{Name: "title", Type: "string", Required: true}}
	out, errs := ValidateParams(declared, map[string]any{"title": ""})
	if len(errs) != 1 || errs[0].Field != "title" || errs[0].Message != "required" {
		t.Fatalf("expected a single 'required' error on title, got %v", errs)
	}
	if _, ok := out["title"]; ok {
		t.Errorf("rejected field should not appear in the coerced map, got %v", out)
	}
}

func TestMissingRequiredParams(t *testing.T) {
	declared := Params{
		{Name: "title", Required: true},
		{Name: "body", Required: true},
		{Name: "priority", Default: "default"},
		{Name: "icon"},
		{Name: "root", Required: true, Default: "${DATADIR}/blobs"},
	}

	tests := []struct {
		name      string
		overrides map[string]string
		want      []string
	}{
		{"none supplied", nil, []string{"title", "body"}},
		{"empty value counts as missing", map[string]string{"title": "", "body": "x"}, []string{"title"}},
		{"overrides satisfy", map[string]string{"title": "t", "body": "b"}, nil},
		{"declared default satisfies an absent override", map[string]string{"title": "t", "body": "b", "root": ""}, []string{"root"}},
		{"undeclared keys are ignored", map[string]string{"title": "t", "body": "b", "event": "approval_pending"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MissingRequiredParams(declared, tc.overrides)
			if len(got) != len(tc.want) {
				t.Fatalf("MissingRequiredParams = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("MissingRequiredParams = %v, want %v (declaration order)", got, tc.want)
				}
			}
		})
	}
}

// TestMissingRequiredParams_EmptyOverrideClobbersDefault pins agreement with
// runtime.MergeParams: an override is applied wherever the key is present, so
// an empty one replaces the declared default rather than falling back to it.
// Counting the param as satisfied here would hand the task "" — the state
// required is meant to exclude.
func TestMissingRequiredParams_EmptyOverrideClobbersDefault(t *testing.T) {
	declared := Params{{Name: "body", Required: true, Default: "from the spec"}}

	if got := MissingRequiredParams(declared, nil); got != nil {
		t.Errorf("absent override should fall back to the default, got missing=%v", got)
	}
	got := MissingRequiredParams(declared, map[string]string{"body": ""})
	if len(got) != 1 || got[0] != "body" {
		t.Errorf("empty override should be missing, got %v", got)
	}
}

func TestValidateSuppliedParams_RequiredMissingAccepted(t *testing.T) {
	declared := Params{
		{Name: "repo", Type: "string", Required: true},
	}
	out, errs := ValidateSuppliedParams(declared, map[string]any{})
	if errs != nil {
		t.Fatalf("requiredness should not be enforced: %v", errs)
	}
	if _, ok := out["repo"]; ok {
		t.Errorf("absent param should stay absent, got %v", out)
	}
}

func TestValidateSuppliedParams_RequiredEmptyAccepted(t *testing.T) {
	declared := Params{
		{Name: "title", Type: "string", Required: true},
	}
	out, errs := ValidateSuppliedParams(declared, map[string]any{"title": ""})
	if errs != nil {
		t.Fatalf("empty value for a required param should pass: %v", errs)
	}
	if out["title"] != "" {
		t.Errorf("got %q, want empty", out["title"])
	}
}

func TestValidateSuppliedParams_StillRejectsUnknownAndMistyped(t *testing.T) {
	declared := Params{
		{Name: "limit", Type: "number", Required: true},
	}
	_, errs := ValidateSuppliedParams(declared, map[string]any{
		"limit": "not-a-number",
		"typo":  "x",
	})
	if len(errs) != 2 {
		t.Fatalf("expected an unknown-key error and a type error, got %v", errs)
	}
}

func TestValidateSuppliedParams_FillsDefaults(t *testing.T) {
	declared := Params{
		{Name: "repo", Type: "string", Required: true, Default: "deno/deno"},
	}
	out, errs := ValidateSuppliedParams(declared, map[string]any{})
	if errs != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if out["repo"] != "deno/deno" {
		t.Errorf("got %q, want the declared default", out["repo"])
	}
}
