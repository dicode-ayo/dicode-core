package main

import (
	"testing"
)

const resumeTestSchema = `{
  "type": "object",
  "properties": {
    "project_name": { "type": "string", "title": "Project name" },
    "count":  { "type": "integer" },
    "ratio":  { "type": "number" },
    "notify": { "type": "boolean" },
    "env":    { "type": "string", "enum": ["staging", "prod"] }
  },
  "required": ["project_name", "env"]
}`

func mustParseProps(t *testing.T) []resumePropEntry {
	t.Helper()
	entries, _, err := parseResumeProps([]byte(resumeTestSchema))
	if err != nil {
		t.Fatalf("parseResumeProps: %v", err)
	}
	return entries
}

func TestParseResumeProps_OrderAndRequired(t *testing.T) {
	entries, required, err := parseResumeProps([]byte(resumeTestSchema))
	if err != nil {
		t.Fatalf("parseResumeProps: %v", err)
	}
	wantOrder := []string{"project_name", "count", "ratio", "notify", "env"}
	if len(entries) != len(wantOrder) {
		t.Fatalf("got %d properties, want %d", len(entries), len(wantOrder))
	}
	for i, w := range wantOrder {
		if entries[i].Name != w {
			t.Errorf("property %d = %q, want %q (declaration order not preserved)", i, entries[i].Name, w)
		}
	}
	if !required["project_name"] || !required["env"] {
		t.Errorf("required set = %v, want project_name and env", required)
	}
	if required["count"] {
		t.Errorf("count should not be required")
	}
}

func TestParseResumeProps_EmptySchema(t *testing.T) {
	entries, required, err := parseResumeProps(nil)
	if err != nil {
		t.Fatalf("parseResumeProps(nil): %v", err)
	}
	if len(entries) != 0 || len(required) != 0 {
		t.Fatalf("empty schema should yield no props/required, got %v %v", entries, required)
	}
}

func TestCoerceResumeValue_Types(t *testing.T) {
	cases := []struct {
		typ  string
		raw  string
		want any
	}{
		{"integer", "10", int64(10)},
		{"number", "3.5", 3.5},
		{"boolean", "yes", true},
		{"boolean", "false", false},
		{"string", "hello", "hello"},
		{"", "bare", "bare"},
	}
	for _, c := range cases {
		got, err := coerceResumeValue(resumeProp{Type: c.typ}, c.raw)
		if err != nil {
			t.Fatalf("coerce(%s,%q): %v", c.typ, c.raw, err)
		}
		if got != c.want {
			t.Errorf("coerce(%s,%q) = %#v, want %#v", c.typ, c.raw, got, c.want)
		}
	}
}

func TestCoerceResumeValue_UntypedNumericEnum(t *testing.T) {
	// enum with no `type`: the CLI must match the raw string against the choices
	// and return the entry's original JSON type (number), not a bare string.
	p := resumeProp{Enum: []any{float64(1), float64(2)}}
	got, err := coerceResumeValue(p, "2")
	if err != nil {
		t.Fatalf("coerce untyped numeric enum: %v", err)
	}
	if got != float64(2) {
		t.Errorf("got %#v, want float64(2)", got)
	}
	if _, err := coerceResumeValue(p, "3"); err == nil {
		t.Error("expected error for a value outside the enum")
	}
}

func TestCoerceResumeValue_StringEnum(t *testing.T) {
	p := resumeProp{Type: "string", Enum: []any{"staging", "prod"}}
	got, err := coerceResumeValue(p, "prod")
	if err != nil {
		t.Fatalf("coerce string enum: %v", err)
	}
	if got != "prod" {
		t.Errorf("got %#v, want prod", got)
	}
	if _, err := coerceResumeValue(p, "dev"); err == nil {
		t.Error("expected error for a value outside the enum")
	}
}

func TestCoerceResumeValue_Errors(t *testing.T) {
	if _, err := coerceResumeValue(resumeProp{Type: "integer"}, "abc"); err == nil {
		t.Error("expected error coercing non-integer")
	}
	if _, err := coerceResumeValue(resumeProp{Type: "number"}, "x"); err == nil {
		t.Error("expected error coercing non-number")
	}
	if _, err := coerceResumeValue(resumeProp{Type: "boolean"}, "maybe"); err == nil {
		t.Error("expected error coercing non-boolean")
	}
}

func TestCollectResumeArgs_CoercesByType(t *testing.T) {
	entries := mustParseProps(t)
	got, err := collectResumeArgs(entries, []string{"project_name=api", "count=7", "notify=true", "env=prod"})
	if err != nil {
		t.Fatalf("collectResumeArgs: %v", err)
	}
	if got["project_name"] != "api" {
		t.Errorf("project_name = %#v", got["project_name"])
	}
	if got["count"] != int64(7) {
		t.Errorf("count = %#v, want int64(7)", got["count"])
	}
	if got["notify"] != true {
		t.Errorf("notify = %#v, want true", got["notify"])
	}
	if got["env"] != "prod" {
		t.Errorf("env = %#v", got["env"])
	}
}

func TestCollectResumeArgs_ValueWithEquals(t *testing.T) {
	got, err := collectResumeArgs(nil, []string{"expr=a=b"})
	if err != nil {
		t.Fatalf("collectResumeArgs: %v", err)
	}
	// Unknown field, only the first '=' splits; passes through as a string.
	if got["expr"] != "a=b" {
		t.Errorf("expr = %#v, want a=b", got["expr"])
	}
}

func TestCollectResumeArgs_MissingEqualsIsError(t *testing.T) {
	if _, err := collectResumeArgs(nil, []string{"approve"}); err == nil {
		t.Fatal("expected error for arg without '='")
	}
}

func TestCollectResumeArgs_BadTypeIsError(t *testing.T) {
	entries := mustParseProps(t)
	if _, err := collectResumeArgs(entries, []string{"count=notanumber"}); err == nil {
		t.Fatal("expected error coercing count=notanumber")
	}
}
