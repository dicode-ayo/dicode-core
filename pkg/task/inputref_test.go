package task

import (
	"errors"
	"reflect"
	"testing"
)

func TestResolveInputOutputMap_PassThroughWithoutToken(t *testing.T) {
	params := map[string]any{
		"path": "/foo/bar",
		"mode": "0600",
	}
	got, err := ResolveInputOutputMap(params, "ignored")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, params) {
		t.Errorf("expected unchanged map; got %v", got)
	}
}

func TestResolveInputOutputMap_SubstitutesExactToken(t *testing.T) {
	params := map[string]any{
		"content": "${input.output}",
		"path":    "/foo/bar",
	}
	got, err := ResolveInputOutputMap(params, "rendered yaml here")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["content"] != "rendered yaml here" {
		t.Errorf("content = %q; want %q", got["content"], "rendered yaml here")
	}
	if got["path"] != "/foo/bar" {
		t.Errorf("path = %q; want %q", got["path"], "/foo/bar")
	}
}

func TestResolveInputOutputMap_EmbeddedTokenIsLiteral(t *testing.T) {
	params := map[string]any{
		"content": "prefix-${input.output}-suffix",
	}
	got, err := ResolveInputOutputMap(params, "X")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["content"] != "prefix-${input.output}-suffix" {
		t.Errorf("embedded token should remain literal; got %q", got["content"])
	}
}

func TestResolveInputOutputMap_RejectsNoUpstream(t *testing.T) {
	params := map[string]any{
		"content": "${input.output}",
	}
	_, err := ResolveInputOutputMap(params, "")
	if err == nil {
		t.Fatal("expected error for missing upstream")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Errorf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
	if ire.Param != "content" {
		t.Errorf("ErrInputUnavailable.Param = %q; want %q", ire.Param, "content")
	}
}

func TestResolveInputOutputMap_NonStringValueUnchanged(t *testing.T) {
	params := map[string]any{
		"count": 42,
		"text":  "${input.output}",
	}
	got, err := ResolveInputOutputMap(params, "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["count"] != 42 {
		t.Errorf("non-string param should be unchanged; got %v", got["count"])
	}
	if got["text"] != "hi" {
		t.Errorf("string param should resolve; got %v", got["text"])
	}
}

func TestResolveInputOutputMap_PreservesInput(t *testing.T) {
	orig := map[string]any{
		"content": "${input.output}",
	}
	_, err := ResolveInputOutputMap(orig, "resolved")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if orig["content"] != "${input.output}" {
		t.Errorf("input map was mutated; got %v", orig["content"])
	}
}

func TestResolveInputOutputList_PassThroughWithoutToken(t *testing.T) {
	params := ParamOverrides{
		{Name: "path", Default: "/foo/bar"},
		{Name: "mode", Default: "0600"},
	}
	got, err := ResolveInputOutputList(params, "ignored")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, params) {
		t.Errorf("expected unchanged list; got %v", got)
	}
}

func TestResolveInputOutputList_SubstitutesExactToken(t *testing.T) {
	params := ParamOverrides{
		{Name: "content", Default: "${input.output}"},
		{Name: "path", Default: "/foo/bar"},
	}
	got, err := ResolveInputOutputList(params, "rendered yaml here")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Default != "rendered yaml here" {
		t.Errorf("got[0].Default = %q; want %q", got[0].Default, "rendered yaml here")
	}
	if got[1].Default != "/foo/bar" {
		t.Errorf("got[1].Default = %q; want %q", got[1].Default, "/foo/bar")
	}
}

func TestResolveInputOutputList_EmbeddedTokenIsLiteral(t *testing.T) {
	params := ParamOverrides{
		{Name: "content", Default: "prefix-${input.output}-suffix"},
	}
	got, err := ResolveInputOutputList(params, "X")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Default != "prefix-${input.output}-suffix" {
		t.Errorf("embedded token should remain literal; got %q", got[0].Default)
	}
}

func TestResolveInputOutputList_RejectsNoUpstream(t *testing.T) {
	params := ParamOverrides{
		{Name: "content", Default: "${input.output}"},
	}
	_, err := ResolveInputOutputList(params, "")
	if err == nil {
		t.Fatal("expected error for missing upstream")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Errorf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
	if ire.Param != "content" {
		t.Errorf("ErrInputUnavailable.Param = %q; want %q", ire.Param, "content")
	}
}

func TestResolveInputOutputList_PreservesInput(t *testing.T) {
	orig := ParamOverrides{
		{Name: "content", Default: "${input.output}"},
	}
	_, err := ResolveInputOutputList(orig, "resolved")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if orig[0].Default != "${input.output}" {
		t.Errorf("input list was mutated; got %v", orig[0].Default)
	}
}
