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
