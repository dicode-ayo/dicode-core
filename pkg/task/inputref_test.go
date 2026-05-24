package task

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// ctxString builds an InputContext for the most common test case —
// a string upstream return value with no params.
func ctxString(s string) InputContext {
	return InputContext{Output: s}
}

func TestResolveInputOutputMap_PassThroughWithoutToken(t *testing.T) {
	params := map[string]any{
		"path": "/foo/bar",
		"mode": "0600",
	}
	got, err := ResolveInputOutputMap(params, ctxString("ignored"))
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
	got, err := ResolveInputOutputMap(params, ctxString("rendered yaml here"))
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

// TestResolveInputOutputMap_EmbeddedTokenInterpolates verifies the
// post-#316 contract: a string containing the recognised token is
// rewritten in place. Was a literal pass-through in PR #310.
func TestResolveInputOutputMap_EmbeddedTokenInterpolates(t *testing.T) {
	params := map[string]any{
		"content": "prefix-${input.output}-suffix",
	}
	got, err := ResolveInputOutputMap(params, ctxString("X"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["content"] != "prefix-X-suffix" {
		t.Errorf("embedded token should be interpolated; got %q", got["content"])
	}
}

func TestResolveInputOutputMap_RejectsNoUpstream(t *testing.T) {
	params := map[string]any{
		"content": "${input.output}",
	}
	_, err := ResolveInputOutputMap(params, InputContext{})
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
	got, err := ResolveInputOutputMap(params, ctxString("hi"))
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
	_, err := ResolveInputOutputMap(orig, ctxString("resolved"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if orig["content"] != "${input.output}" {
		t.Errorf("input map was mutated; got %v", orig["content"])
	}
}

// TestResolveInputOutputMap_OutputField_StringOK exercises the
// ${input.output.<field>} form with a map[string]any upstream return —
// the shape JSON decoding produces. The named field's string value is
// substituted.
func TestResolveInputOutputMap_OutputField_StringOK(t *testing.T) {
	params := map[string]any{
		"file": "${input.output.path}",
	}
	ctx := InputContext{Output: map[string]any{"path": "/tmp/rendered.yaml"}}
	got, err := ResolveInputOutputMap(params, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["file"] != "/tmp/rendered.yaml" {
		t.Errorf("file = %q; want %q", got["file"], "/tmp/rendered.yaml")
	}
}

// TestResolveInputOutputMap_OutputField_NonStringFails locks the loud
// failure: a present-but-non-string field yields ErrInputUnavailable.
func TestResolveInputOutputMap_OutputField_NonStringFails(t *testing.T) {
	params := map[string]any{
		"size": "${input.output.bytes}",
	}
	ctx := InputContext{Output: map[string]any{"bytes": 42}}
	_, err := ResolveInputOutputMap(params, ctx)
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for non-string field value")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Fatalf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
	if !strings.Contains(ire.Error(), "not a string") {
		t.Errorf("error should mention non-string; got %v", ire)
	}
}

// TestResolveInputOutputMap_OutputField_MissingFails locks the loud
// failure: a referenced field absent from the upstream map yields
// ErrInputUnavailable.
func TestResolveInputOutputMap_OutputField_MissingFails(t *testing.T) {
	params := map[string]any{
		"x": "${input.output.missing}",
	}
	ctx := InputContext{Output: map[string]any{"path": "/tmp/foo"}}
	_, err := ResolveInputOutputMap(params, ctx)
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for missing field")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Fatalf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
	if !strings.Contains(ire.Error(), `field "missing"`) {
		t.Errorf("error should name the missing field; got %v", ire)
	}
}

// TestResolveInputOutputMap_OutputField_NonObjectFails pins the loud
// failure when the upstream isn't object-shaped at all (e.g. an
// upstream that returned a string but a downstream tries to address a
// field on it).
func TestResolveInputOutputMap_OutputField_NonObjectFails(t *testing.T) {
	params := map[string]any{
		"x": "${input.output.path}",
	}
	ctx := InputContext{Output: "this is a string, not an object"}
	_, err := ResolveInputOutputMap(params, ctx)
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for non-object upstream")
	}
}

// TestResolveInputOutputMap_Params_OK pins ${input.params.<name>}
// against a populated upstream Params map.
func TestResolveInputOutputMap_Params_OK(t *testing.T) {
	params := map[string]any{
		"endpoint": "${input.params.url}",
	}
	ctx := InputContext{Params: map[string]string{"url": "https://example.com"}}
	got, err := ResolveInputOutputMap(params, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["endpoint"] != "https://example.com" {
		t.Errorf("endpoint = %q; want %q", got["endpoint"], "https://example.com")
	}
}

// TestResolveInputOutputMap_Params_MissingFails locks the loud failure
// when the named param is absent from the upstream's Params map.
func TestResolveInputOutputMap_Params_MissingFails(t *testing.T) {
	params := map[string]any{
		"endpoint": "${input.params.url}",
	}
	ctx := InputContext{Params: map[string]string{"other": "x"}}
	_, err := ResolveInputOutputMap(params, ctx)
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for missing param")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Fatalf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
}

// TestResolveInputOutputMap_Params_NilMapFails locks the loud failure
// when the upstream has no Params map at all. A `${input.params.X}`
// reference must error rather than substitute an empty string.
func TestResolveInputOutputMap_Params_NilMapFails(t *testing.T) {
	params := map[string]any{
		"endpoint": "${input.params.url}",
	}
	_, err := ResolveInputOutputMap(params, InputContext{Output: "x"})
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for nil Params")
	}
}

// TestResolveInputOutputMap_HyphenatedAndDottedIdents exercises the
// widened identifier class (PR #330 follow-up). Hyphenated names
// (`x-forwarded-for`, `db-host`) and dotted names (`db.host`) must
// resolve against the upstream params map / output map exactly like
// plain identifiers — operators legitimately use those shapes for
// HTTP-header-style param names and namespaced YAML keys.
func TestResolveInputOutputMap_HyphenatedAndDottedIdents(t *testing.T) {
	params := map[string]any{
		"hdr":  "${input.params.x-forwarded-for}",
		"db":   "${input.output.db-host}",
		"port": "${input.params.db.host}",
	}
	ctx := InputContext{
		Output: map[string]any{"db-host": "db.internal"},
		Params: map[string]string{
			"x-forwarded-for": "10.0.0.1",
			"db.host":         "primary",
		},
	}
	got, err := ResolveInputOutputMap(params, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["hdr"] != "10.0.0.1" {
		t.Errorf("hdr = %q; want %q", got["hdr"], "10.0.0.1")
	}
	if got["db"] != "db.internal" {
		t.Errorf("db = %q; want %q", got["db"], "db.internal")
	}
	if got["port"] != "primary" {
		t.Errorf("port = %q; want %q", got["port"], "primary")
	}
}

// TestResolveInputOutputMap_MultiToken pins multi-token interpolation
// — e.g. constructing a URL from two distinct references. Each token
// resolves independently against the same InputContext.
func TestResolveInputOutputMap_MultiToken(t *testing.T) {
	params := map[string]any{
		"url": "${input.params.scheme}://${input.output.host}/path",
	}
	ctx := InputContext{
		Output: map[string]any{"host": "example.com"},
		Params: map[string]string{"scheme": "https"},
	}
	got, err := ResolveInputOutputMap(params, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["url"] != "https://example.com/path" {
		t.Errorf("url = %q; want %q", got["url"], "https://example.com/path")
	}
}

func TestResolveInputOutputList_PassThroughWithoutToken(t *testing.T) {
	params := ParamOverrides{
		{Name: "path", Default: "/foo/bar"},
		{Name: "mode", Default: "0600"},
	}
	got, err := ResolveInputOutputList(params, ctxString("ignored"))
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
	got, err := ResolveInputOutputList(params, ctxString("rendered yaml here"))
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

// TestResolveInputOutputList_EmbeddedTokenInterpolates is the slice-
// shaped twin of TestResolveInputOutputMap_EmbeddedTokenInterpolates.
func TestResolveInputOutputList_EmbeddedTokenInterpolates(t *testing.T) {
	params := ParamOverrides{
		{Name: "content", Default: "prefix-${input.output}-suffix"},
	}
	got, err := ResolveInputOutputList(params, ctxString("X"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Default != "prefix-X-suffix" {
		t.Errorf("embedded token should be interpolated; got %q", got[0].Default)
	}
}

func TestResolveInputOutputList_RejectsNoUpstream(t *testing.T) {
	params := ParamOverrides{
		{Name: "content", Default: "${input.output}"},
	}
	_, err := ResolveInputOutputList(params, InputContext{})
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
	_, err := ResolveInputOutputList(orig, ctxString("resolved"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if orig[0].Default != "${input.output}" {
		t.Errorf("input list was mutated; got %v", orig[0].Default)
	}
}

// TestResolveInputOutputList_OutputField_Pipeline pins the slice-shaped
// equivalent of TestResolveInputOutputMap_OutputField_StringOK — this is
// the form used by kind: PipelineTask stage overrides.params at dispatch.
func TestResolveInputOutputList_OutputField_Pipeline(t *testing.T) {
	params := ParamOverrides{
		{Name: "destPath", Default: "${input.output.path}"},
		{Name: "mode", Default: "0600"},
	}
	ctx := InputContext{Output: map[string]any{"path": "/var/run/relay.yaml"}}
	got, err := ResolveInputOutputList(params, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].Default != "/var/run/relay.yaml" {
		t.Errorf("destPath = %q; want %q", got[0].Default, "/var/run/relay.yaml")
	}
	if got[1].Default != "0600" {
		t.Errorf("mode = %q; want %q", got[1].Default, "0600")
	}
}

// TestResolveInputOutputMap_NilMap pins that a nil params map is a no-op:
// no error and the returned map is also nil. Both call sites (FireChain
// and runPrereqs) can legitimately reach the resolver with a nil map
// (chain.Params or overrides.Params omitted), and they rely on this
// short-circuit to avoid an unnecessary allocation + the surrounding
// dispatch logic should treat the result as "nothing to substitute".
func TestResolveInputOutputMap_NilMap(t *testing.T) {
	got, err := ResolveInputOutputMap(nil, ctxString("anything"))
	if err != nil {
		t.Fatalf("nil map should not error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result; got %v", got)
	}
}

// TestResolveInputOutputList_NilList is the slice-shaped counterpart to
// TestResolveInputOutputMap_NilMap. The pipeline-stage resolver passes
// stage.Overrides.Params straight through to ResolveInputOutputList; that
// field is nil when the override declares no params, so the helper must
// accept nil without error.
func TestResolveInputOutputList_NilList(t *testing.T) {
	got, err := ResolveInputOutputList(nil, ctxString("anything"))
	if err != nil {
		t.Fatalf("nil list should not error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result; got %v", got)
	}
}

// TestResolveString_RejectsEmptyOutput pins the existing loud failure
// for the bare `${input.output}` form: an upstream that returns an
// empty string is treated as "not provided" and yields
// ErrInputUnavailable. This is the pre-existing contract from PR #310,
// captured here so the regression net covers all three forms together.
func TestResolveString_RejectsEmptyOutput(t *testing.T) {
	params := map[string]any{
		"content": "${input.output}",
	}
	_, err := ResolveInputOutputMap(params, ctxString(""))
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for empty upstream string")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Fatalf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
	if ire.Param != "content" {
		t.Errorf("ErrInputUnavailable.Param = %q; want %q", ire.Param, "content")
	}
}

// TestResolveString_RejectsEmptyOutputField locks the symmetric loud
// failure for the `${input.output.<field>}` form: a present-but-empty
// string field is treated as "not provided" rather than substituted
// silently. Mirrors TestResolveString_RejectsEmptyOutput for the
// dotted-form to preserve the Failed-loudly contract uniformly. Issue
// #333.
func TestResolveString_RejectsEmptyOutputField(t *testing.T) {
	params := map[string]any{
		"file": "${input.output.path}",
	}
	ctx := InputContext{Output: map[string]any{"path": ""}}
	_, err := ResolveInputOutputMap(params, ctx)
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for empty output field")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Fatalf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
	if ire.Param != "file" {
		t.Errorf("ErrInputUnavailable.Param = %q; want %q", ire.Param, "file")
	}
	if ire.Ref != "${input.output.path}" {
		t.Errorf("ErrInputUnavailable.Ref = %q; want %q", ire.Ref, "${input.output.path}")
	}
	if !strings.Contains(ire.Error(), "empty string") {
		t.Errorf("error should mention empty-string contract; got %v", ire)
	}
}

// TestResolveString_RejectsEmptyOutputField_StringMap is the
// map[string]string twin of TestResolveString_RejectsEmptyOutputField —
// runtime-test mocks reach the resolver via map[string]string and that
// path must also fail loudly when the field's value is empty.
func TestResolveString_RejectsEmptyOutputField_StringMap(t *testing.T) {
	params := map[string]any{
		"file": "${input.output.path}",
	}
	ctx := InputContext{Output: map[string]string{"path": ""}}
	_, err := ResolveInputOutputMap(params, ctx)
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for empty output field (map[string]string)")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Fatalf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
	if !strings.Contains(ire.Error(), "empty string") {
		t.Errorf("error should mention empty-string contract; got %v", ire)
	}
}

// TestResolveString_RejectsEmptyParam locks the symmetric loud failure
// for the `${input.params.<name>}` form: a present-but-empty entry in
// the upstream's Params map is treated as "not provided" rather than
// substituted silently. Issue #333.
func TestResolveString_RejectsEmptyParam(t *testing.T) {
	params := map[string]any{
		"endpoint": "${input.params.X}",
	}
	ctx := InputContext{Params: map[string]string{"X": ""}}
	_, err := ResolveInputOutputMap(params, ctx)
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for empty param value")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Fatalf("expected ErrInputUnavailable; got %T: %v", err, err)
	}
	if ire.Param != "endpoint" {
		t.Errorf("ErrInputUnavailable.Param = %q; want %q", ire.Param, "endpoint")
	}
	if ire.Ref != "${input.params.X}" {
		t.Errorf("ErrInputUnavailable.Ref = %q; want %q", ire.Ref, "${input.params.X}")
	}
	if !strings.Contains(ire.Error(), "empty string") {
		t.Errorf("error should mention empty-string contract; got %v", ire)
	}
}

// TestResolveString_RejectsEmptyEmbedded pins the loud-failure contract
// for embedded `${input.…}` tokens: when a single token inside an
// otherwise-valid embedded form ("prefix-${input.output}-suffix") would
// resolve to an empty string, the resolver fails the entire param the
// same way the bare form does — resolveString short-circuits on the
// first token error, so an empty substitution can't silently produce
// "prefix--suffix". Companion to TestResolveString_RejectsEmptyOutput,
// kept separate so the embedded-form contract is greppable. Issue #333.
func TestResolveString_RejectsEmptyEmbedded(t *testing.T) {
	params := map[string]any{
		"endpoint": "https://${input.output}/api",
	}
	_, err := ResolveInputOutputMap(params, ctxString(""))
	if err == nil {
		t.Fatal("expected ErrInputUnavailable for empty upstream in embedded form")
	}
	var ire *ErrInputUnavailable
	if !errors.As(err, &ire) {
		t.Fatalf("expected *ErrInputUnavailable; got %T: %v", err, err)
	}
	if ire.Param != "endpoint" {
		t.Errorf("ErrInputUnavailable.Param = %q; want %q", ire.Param, "endpoint")
	}
	if !strings.Contains(ire.Error(), "empty string") {
		t.Errorf("error should mention empty-string contract; got %v", ire)
	}
}

// TestValidateInputRefs covers the registration-time gate that
// surfaces malformed `${input.…}` shapes at config load. Well-formed
// references (the three recognised shapes) and strings without any
// `${input.…}` substring pass; everything else errors with a site-
// qualified message.
func TestValidateInputRefs(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		wantSub string // expected substring of err.Error()
	}{
		{"empty", "", false, ""},
		{"plain literal", "/tmp/foo", false, ""},
		{"bare output", "${input.output}", false, ""},
		{"output field", "${input.output.path}", false, ""},
		{"params name", "${input.params.url}", false, ""},
		{"embedded", "prefix-${input.output}-suffix", false, ""},
		{"multi-token", "${input.params.scheme}://${input.output.host}", false, ""},
		// Widened identifier class (PR #330 follow-up): hyphenated names
		// (common for HTTP header values like `x-forwarded-for`) and
		// dotted names (common for namespaced YAML keys like `db.host`)
		// must both be accepted — the upstream params map is itself
		// indexed by arbitrary string keys, so this resolver shouldn't
		// be narrower than its lookup target.
		{"params hyphenated", "${input.params.x-forwarded-for}", false, ""},
		{"output hyphenated", "${input.output.db-host}", false, ""},
		{"params dotted", "${input.params.db.host}", false, ""},
		{"output dotted multi", "${input.output.a.b}", false, ""},
		{"unknown kind", "${input.foo}", true, "${input.foo}"},
		{"params no field", "${input.params}", true, "${input.params}"},
		{"output non-identifier", "${input.output.0bad}", true, "${input.output.0bad}"},
		// Leading char is still strict — must be letter or underscore.
		{"output leading hyphen", "${input.output.-foo}", true, "${input.output.-foo}"},
		{"output leading dot", "${input.output..foo}", true, "${input.output..foo}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateInputRefs("site.x", c.in)
			if c.wantErr && err == nil {
				t.Fatalf("expected error for %q", c.in)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", c.in, err)
			}
			if err != nil && c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q should mention %q", err.Error(), c.wantSub)
			}
			if err != nil && !strings.Contains(err.Error(), "site.x") {
				t.Errorf("error should include site; got %v", err)
			}
		})
	}
}
