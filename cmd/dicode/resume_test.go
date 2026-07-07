package main

import (
	"encoding/json"
	"testing"
)

func TestParseResumeInput_KeyValueToJSON(t *testing.T) {
	got, err := parseResumeInput([]string{"approve=yes", "note=looks good"})
	if err != nil {
		t.Fatalf("parseResumeInput: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["approve"] != "yes" {
		t.Errorf("approve = %q, want yes", m["approve"])
	}
	// Values may contain `=` and spaces — only the first `=` splits.
	if m["note"] != "looks good" {
		t.Errorf("note = %q, want 'looks good'", m["note"])
	}
}

func TestParseResumeInput_ValueWithEquals(t *testing.T) {
	got, err := parseResumeInput([]string{"expr=a=b"})
	if err != nil {
		t.Fatalf("parseResumeInput: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["expr"] != "a=b" {
		t.Errorf("expr = %q, want a=b", m["expr"])
	}
}

func TestParseResumeInput_EmptyIsEmptyObject(t *testing.T) {
	got, err := parseResumeInput(nil)
	if err != nil {
		t.Fatalf("parseResumeInput: %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("got %s, want {}", got)
	}
}

func TestParseResumeInput_MissingEqualsIsError(t *testing.T) {
	if _, err := parseResumeInput([]string{"approve"}); err == nil {
		t.Fatal("expected error for arg without '='")
	}
}
