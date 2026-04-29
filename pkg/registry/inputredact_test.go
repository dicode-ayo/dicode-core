package registry

import "testing"

func TestShouldRedactName_ExactMatches(t *testing.T) {
	cases := []string{
		"Authorization",
		"authorization",
		"Cookie",
		"X-Hub-Signature-256",
		"X-Slack-Signature",
		"password",
		"PASSWORD",
		"api_key",
		"APIKEY",
		"bearer",
	}
	for _, name := range cases {
		if !shouldRedactName(name) {
			t.Errorf("shouldRedactName(%q) = false, want true", name)
		}
	}
}

func TestShouldRedactName_SubstringMatches(t *testing.T) {
	cases := []string{
		"x-custom-token",
		"MY_SLACK_TOKEN",
		"gh-secret-foo",
		"my_password_field",
		"public_key",
		"x-stripe-signature-v2",
	}
	for _, name := range cases {
		if !shouldRedactName(name) {
			t.Errorf("shouldRedactName(%q) = false, want true (substring match)", name)
		}
	}
}

func TestShouldRedactName_OverRedaction(t *testing.T) {
	// "tokens_per_minute" matches "token" substring — documented safe failure.
	if !shouldRedactName("tokens_per_minute") {
		t.Errorf("shouldRedactName(tokens_per_minute) = false; expected true (over-redaction is safe)")
	}
}

func TestShouldRedactName_NonMatches(t *testing.T) {
	cases := []string{
		"User-Agent",
		"Content-Type",
		"X-Request-ID",
		"name",
		"username",
		"email",
		"created_at",
	}
	for _, name := range cases {
		if shouldRedactName(name) {
			t.Errorf("shouldRedactName(%q) = true, want false", name)
		}
	}
}

func TestRedactHeaders_RedactsMatchingNames(t *testing.T) {
	in := map[string][]string{
		"Authorization":  {"Bearer xyz"},
		"X-Custom-Token": {"abc", "def"},
		"User-Agent":     {"Mozilla/5.0"},
		"Content-Type":   {"application/json"},
	}
	redacted := []string{}
	out := redactHeaders(in, &redacted)

	if got := out["Authorization"][0]; got != redactPlaceholder {
		t.Errorf("Authorization not redacted: %q", got)
	}
	if got := out["X-Custom-Token"][0]; got != redactPlaceholder {
		t.Errorf("X-Custom-Token[0] not redacted: %q", got)
	}
	if got := out["X-Custom-Token"][1]; got != redactPlaceholder {
		t.Errorf("X-Custom-Token[1] not redacted: %q", got)
	}
	if got := out["User-Agent"][0]; got != "Mozilla/5.0" {
		t.Errorf("User-Agent should not be redacted: %q", got)
	}
	if got := out["Content-Type"][0]; got != "application/json" {
		t.Errorf("Content-Type should not be redacted: %q", got)
	}

	wantPaths := map[string]bool{
		"headers.Authorization":  true,
		"headers.X-Custom-Token": true,
	}
	got := map[string]bool{}
	for _, p := range redacted {
		got[p] = true
	}
	if len(got) != len(wantPaths) {
		t.Errorf("redacted = %v, want exactly %v", got, wantPaths)
	}
	for k := range wantPaths {
		if !got[k] {
			t.Errorf("expected %q in redacted, got %v", k, redacted)
		}
	}
}

func TestRedactHeaders_DoesNotMutateInput(t *testing.T) {
	in := map[string][]string{
		"Authorization": {"Bearer xyz"},
	}
	redacted := []string{}
	_ = redactHeaders(in, &redacted)
	// Original map must be untouched.
	if in["Authorization"][0] != "Bearer xyz" {
		t.Errorf("input mutated: in[Authorization][0] = %q", in["Authorization"][0])
	}
}

func TestRedactHeaders_NilAndEmpty(t *testing.T) {
	redacted := []string{}
	out := redactHeaders(nil, &redacted)
	if out != nil && len(out) != 0 {
		t.Errorf("nil input should produce nil/empty output, got %v", out)
	}
	out = redactHeaders(map[string][]string{}, &redacted)
	if len(out) != 0 {
		t.Errorf("empty input should produce empty output, got %v", out)
	}
	if len(redacted) != 0 {
		t.Errorf("nothing should have been added to redacted: %v", redacted)
	}
}

func TestRedactQuery_PathPrefix(t *testing.T) {
	in := map[string][]string{
		"page":    {"1"},
		"api_key": {"sk_xyz"},
	}
	redacted := []string{}
	out := redactQuery(in, &redacted)

	if out["api_key"][0] != redactPlaceholder {
		t.Errorf("api_key not redacted: %q", out["api_key"][0])
	}
	if out["page"][0] != "1" {
		t.Errorf("page should not be redacted: %q", out["page"][0])
	}
	wantPath := "query.api_key"
	found := false
	for _, p := range redacted {
		if p == wantPath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %q in redacted, got %v", wantPath, redacted)
	}
}

func TestRedactParams_RecursiveMaps(t *testing.T) {
	in := map[string]any{
		"name": "Alice",
		"creds": map[string]any{
			"password": "secret123",
			"username": "alice",
		},
	}
	redacted := []string{}
	out := redactParams(in, "params", &redacted).(map[string]any)

	if out["name"] != "Alice" {
		t.Errorf("name should not be redacted: %v", out["name"])
	}
	creds := out["creds"].(map[string]any)
	if creds["password"] != redactPlaceholder {
		t.Errorf("creds.password not redacted: %v", creds["password"])
	}
	if creds["username"] != "alice" {
		t.Errorf("creds.username should not be redacted: %v", creds["username"])
	}

	wantPath := "params.creds.password"
	found := false
	for _, p := range redacted {
		if p == wantPath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %q in redacted, got %v", wantPath, redacted)
	}
}

func TestRedactParams_PositionalLists(t *testing.T) {
	in := map[string]any{
		"items": []any{
			map[string]any{"id": float64(1), "token": "t1"},
			map[string]any{"id": float64(2), "token": "t2"},
		},
	}
	redacted := []string{}
	out := redactParams(in, "params", &redacted).(map[string]any)

	items := out["items"].([]any)
	for i, raw := range items {
		item := raw.(map[string]any)
		if item["token"] != redactPlaceholder {
			t.Errorf("items[%d].token not redacted: %v", i, item["token"])
		}
	}

	wantPaths := []string{
		"params.items[0].token",
		"params.items[1].token",
	}
	for _, want := range wantPaths {
		found := false
		for _, p := range redacted {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in redacted, got %v", want, redacted)
		}
	}
}

func TestRedactParams_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{
		"creds": map[string]any{"password": "secret"},
	}
	redacted := []string{}
	_ = redactParams(in, "params", &redacted)
	creds := in["creds"].(map[string]any)
	if creds["password"] != "secret" {
		t.Errorf("input mutated: creds.password = %v", creds["password"])
	}
}

func TestRedactParams_PrimitiveAndNil(t *testing.T) {
	redacted := []string{}
	if got := redactParams("hello", "params", &redacted); got != "hello" {
		t.Errorf("primitive should pass through: %v", got)
	}
	if got := redactParams(nil, "params", &redacted); got != nil {
		t.Errorf("nil should pass through: %v", got)
	}
	if len(redacted) != 0 {
		t.Errorf("primitives should produce no redaction entries")
	}
}

func TestRedactParams_DepthGuard(t *testing.T) {
	// Build a 100-deep nested map — exceeds maxRedactionDepth (64).
	var v any = "leaf"
	for i := 0; i < 100; i++ {
		v = map[string]any{"k": v}
	}
	redacted := []string{}
	out := redactParams(v, "params", &redacted)
	// Walk the result and confirm we hit the depth-guard placeholder somewhere
	// before reaching the leaf.
	cur := out
	for i := 0; i < 100; i++ {
		s, isStr := cur.(string)
		if isStr {
			// We reached a string: should be either the depth-guard placeholder
			// or the original leaf (if depth guard wasn't needed, which can't
			// happen here since depth 100 > maxRedactionDepth 64).
			if s != "<redacted-too-deep>" && s != "leaf" {
				t.Errorf("unexpected string at depth %d: %q", i, s)
			}
			return
		}
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("unexpected type at depth %d: %T", i, cur)
		}
		cur = m["k"]
	}
	// If we exit the loop without hitting a string, the depth guard never fired.
	t.Errorf("depth guard never triggered; walked all 100 levels without hitting placeholder")
}
