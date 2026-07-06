package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/taskset"
)

// ── 1. apiGetConfigRaw redaction ──────────────────────────────────────────────

func TestAPIGetConfigRaw_RedactsSecret(t *testing.T) {
	// Write a temp dicode.yaml that contains a secret.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	yamlContent := `server:
  port: 8080
  auth: true
  secret: mysecret
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	minCfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	srv, _ := newTestServerWithSourceMgr(t, minCfg, cfgPath, nil)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/config/raw", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	content := resp["content"]

	if strings.Contains(content, "mysecret") {
		t.Errorf("response contains secret value 'mysecret' — it should be redacted")
	}
	if !strings.Contains(content, "server") {
		t.Errorf("response does not contain 'server' key — other fields should be preserved")
	}
}

func TestAPIGetConfigRaw_PreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	yamlContent := `server:
  port: 9999
  auth: false
  secret: topsecret
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	minCfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	srv, _ := newTestServerWithSourceMgr(t, minCfg, cfgPath, nil)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/config/raw", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	content := resp["content"]

	if strings.Contains(content, "topsecret") {
		t.Errorf("secret should be redacted, found in content")
	}
	if !strings.Contains(content, "server") {
		t.Errorf("server key should be present in response")
	}
}

func TestRedactServerSecret_PreservesComments(t *testing.T) {
	input := "server:\n  auth: true\n  secret: mysecret\n# trailing-comment\n"
	out := string(redactServerSecret([]byte(input)))
	if strings.Contains(out, "mysecret") {
		t.Error("secret was not redacted")
	}
	if !strings.Contains(out, "# trailing-comment") {
		t.Error("comment was lost during redaction")
	}
}

func TestRedactServerSecret_PreservesInlineComment(t *testing.T) {
	input := "server:\n  port: 8080\n  secret: pass\n  # my-marker\n"
	out := string(redactServerSecret([]byte(input)))
	if strings.Contains(out, "pass") {
		t.Error("secret was not redacted")
	}
	if !strings.Contains(out, "# my-marker") {
		t.Error("inline comment inside server block was lost")
	}
}

func TestRedactServerSecret_BlockScalar(t *testing.T) {
	input := "server:\n  auth: true\n  secret: |\n    line-one-LEAK\n    line-two-LEAK\n  port: 8080\n"
	out := string(redactServerSecret([]byte(input)))
	if strings.Contains(out, "LEAK") {
		t.Errorf("block-scalar secret continuation leaked:\n%s", out)
	}
	if !strings.Contains(out, "port: 8080") {
		t.Errorf("field after block scalar was lost:\n%s", out)
	}
}

func TestRedactServerSecret_BlockScalarChomp(t *testing.T) {
	input := "server:\n  secret: >-\n    folded-LEAK\n  auth: true\n"
	out := string(redactServerSecret([]byte(input)))
	if strings.Contains(out, "LEAK") {
		t.Errorf("folded/chomped block-scalar secret leaked:\n%s", out)
	}
	if !strings.Contains(out, "auth: true") {
		t.Errorf("field after block scalar was lost:\n%s", out)
	}
}

func TestRedactServerSecret_InlineFlow(t *testing.T) {
	input := "server: {auth: true, secret: inlineLEAK, port: 8080}\n"
	out := string(redactServerSecret([]byte(input)))
	if strings.Contains(out, "inlineLEAK") {
		t.Errorf("inline-flow secret leaked:\n%s", out)
	}
	if !strings.Contains(out, "auth: true") || !strings.Contains(out, "port: 8080") {
		t.Errorf("sibling flow fields were lost:\n%s", out)
	}
}

func TestRedactServerSecret_MultiLineFlow(t *testing.T) {
	input := "server: {\n  auth: true,\n  secret: spanningLEAK,\n}\n"
	out := string(redactServerSecret([]byte(input)))
	if strings.Contains(out, "spanningLEAK") {
		t.Errorf("multi-line flow secret leaked:\n%s", out)
	}
}

func TestRedactServerSecret_DoesNotOverRedact(t *testing.T) {
	// A non-secret key whose name merely starts with "secret" must survive.
	input := "server:\n  secret_backup: keep-me\n  auth: true\n"
	out := string(redactServerSecret([]byte(input)))
	if !strings.Contains(out, "keep-me") {
		t.Errorf("secret_backup was wrongly redacted:\n%s", out)
	}
}

func TestSecureCookies_DirectTLS(t *testing.T) {
	// Direct TLS (tls_cert + tls_key set, trust_proxy false) must enable Secure.
	cfg := &config.Config{
		Server: config.ServerConfig{
			TrustProxy:  false,
			TLSCertFile: "/etc/ssl/cert.pem",
			TLSKeyFile:  "/etc/ssl/key.pem",
		},
	}
	if !secureCookies(cfg) {
		t.Error("secureCookies should return true when tls_cert+tls_key are set")
	}
}

func TestSecureCookies_TrustProxyOnly(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{TrustProxy: true}}
	if !secureCookies(cfg) {
		t.Error("secureCookies should return true when trust_proxy is true")
	}
}

func TestSecureCookies_NeitherSet(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{}}
	if secureCookies(cfg) {
		t.Error("secureCookies should return false when neither TLS nor proxy is configured")
	}
}

// ── 2. /mcp accepts Bearer API key ───────────────────────────────────────────

func TestMCP_AcceptsBearerAPIKey(t *testing.T) {
	// Build a server with auth enabled.
	srv := newAuthServer(t, "hunter2")

	// Generate a real API key directly through the store.
	ctx := context.Background()
	rawKey, _, err := srv.apiKeys.generate(ctx, "test-key")
	if err != nil {
		t.Fatalf("generate API key: %v", err)
	}

	h := srv.Handler()

	// GET /mcp with a Bearer API key — must not return 401.
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized {
		t.Errorf("GET /mcp with valid Bearer key returned 401 — should be allowed")
	}
}

func TestMCP_Rejects_NoAuth(t *testing.T) {
	// With auth enabled, /mcp without any credential should return 401.
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /mcp without auth: expected 401, got %d", w.Code)
	}
}

// ── 3. SSRF validation — isAllowedGitScheme ──────────────────────────────────

func TestIsAllowedGitScheme_Allowed(t *testing.T) {
	cases := []string{
		"https://github.com/user/repo",
		"http://github.com/user/repo",
		"ssh://git@github.com/user/repo",
	}
	for _, u := range cases {
		if !isAllowedGitScheme(u) {
			t.Errorf("isAllowedGitScheme(%q) = false, want true", u)
		}
	}
}

// git:// is rejected (#486): go-git dials it through a native transport with
// a hardcoded, unguarded net.Dial, so a git:// source added via apiAddSource
// or previewed via apiListGitBranches would get zero SSRF host validation —
// unlike http/https/ssh.
func TestIsAllowedGitScheme_Rejected(t *testing.T) {
	cases := []string{
		"file:///etc/passwd",
		"ftp://example.com/repo",
		"data:text/plain,hello",
		"javascript:alert(1)",
		"",
		"not-a-url",
		"git://github.com/user/repo",
		"git://169.254.169.254/metadata",
	}
	for _, u := range cases {
		if isAllowedGitScheme(u) {
			t.Errorf("isAllowedGitScheme(%q) = true, want false", u)
		}
	}
}

// ── 3b. apiListGitBranches — token_env restriction + SSRF host block (#475) ──

// branchesTestServer builds a minimal Server whose config has one git source
// with an operator-designated credential env var. apiListGitBranches only
// touches cfg/cfgMu, so no full test server (and no deno) is needed.
func branchesTestServer() *Server {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"corp-tasks": {Ref: &taskset.Ref{
			URL:    "https://github.com/corp/tasks.git",
			Branch: "main",
			Auth:   taskset.RefAuth{TokenEnv: "GH_TOKEN"},
		}},
		"local": {Ref: &taskset.Ref{Path: "/tmp/tasks"}},
	}
	return &Server{cfg: cfg}
}

func listBranchesReq(srv *Server, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	srv.apiListGitBranches(w, req)
	return w
}

// TestApiListGitBranches_RejectsPrivateHosts asserts that URLs pointing at
// loopback/private/link-local/internal hosts are rejected with a clear error
// and never reach the dial stage ("list remote" only appears in errors
// wrapped after a connection attempt).
func TestApiListGitBranches_RejectsPrivateHosts(t *testing.T) {
	srv := branchesTestServer()
	cases := []string{
		"http://127.0.0.1/repo.git",
		"https://10.0.0.8/repo.git",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/repo.git",
		"http://localhost:3000/repo.git",
		"http://metadata.google.internal/computeMetadata/v1",
	}
	for _, u := range cases {
		w := listBranchesReq(srv, "/api/settings/sources/git/branches?url="+url.QueryEscape(u))
		if w.Code != http.StatusBadRequest {
			t.Errorf("url=%q: code = %d, want 400", u, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "private or internal") {
			t.Errorf("url=%q: body = %q, want private/internal host rejection", u, body)
		}
		if strings.Contains(body, "list remote") {
			t.Errorf("url=%q: reached the dial stage: %q", u, body)
		}
	}
}

// TestApiListGitBranches_RejectsUnknownTokenEnv asserts that naming an env
// var that is not an operator-configured git credential is refused outright —
// the value must never be attached as basic auth (this was the secret
// exfiltration vector in #475).
func TestApiListGitBranches_RejectsUnknownTokenEnv(t *testing.T) {
	t.Setenv("SUPER_SECRET_TOKEN", "hunter2") // present in the daemon env, still must not be usable
	srv := branchesTestServer()

	w := listBranchesReq(srv,
		"/api/settings/sources/git/branches?url="+url.QueryEscape("https://github.com/evil/exfil.git")+
			"&token_env=SUPER_SECRET_TOKEN")

	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not permitted") {
		t.Errorf("body = %q, want token_env rejection message", w.Body.String())
	}
}

// TestApiListGitBranches_LegitPublicURLPassesGates asserts the two security
// gates do not break the legitimate case: a public host with no token_env
// must get past both checks. The request context is pre-cancelled so go-git
// fails at the dial stage ("list remote") without touching the network.
func TestApiListGitBranches_LegitPublicURLPassesGates(t *testing.T) {
	srv := branchesTestServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodGet,
		"/api/settings/sources/git/branches?url="+url.QueryEscape("https://github.com/example/repo.git"), nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.apiListGitBranches(w, req)

	body := w.Body.String()
	if strings.Contains(body, "private or internal") || strings.Contains(body, "not permitted") {
		t.Fatalf("legitimate request was blocked by a security gate: %s", body)
	}
	if !strings.Contains(body, "list remote") {
		t.Fatalf("expected failure from the dial stage (list remote), got: %s", body)
	}
}

// TestResolveBranchesTokenEnv covers the credential-resolution rule directly:
// configured-source URLs use their own token_env (request input ignored);
// unknown URLs may only use an env var already designated as a git credential.
func TestResolveBranchesTokenEnv(t *testing.T) {
	srv := branchesTestServer()

	cases := []struct {
		name      string
		url       string
		requested string
		want      string
		wantErr   bool
	}{
		{"configured URL uses own credential, request ignored",
			"https://github.com/corp/tasks.git", "SUPER_SECRET_TOKEN", "GH_TOKEN", false},
		{"configured URL matches without .git suffix",
			"https://github.com/corp/tasks", "PATH", "GH_TOKEN", false},
		{"unknown URL, empty token_env allowed",
			"https://github.com/other/repo.git", "", "", false},
		{"unknown URL, operator-designated credential allowed",
			"https://github.com/other/repo.git", "GH_TOKEN", "GH_TOKEN", false},
		{"unknown URL, arbitrary env var rejected",
			"https://github.com/other/repo.git", "AWS_SECRET_ACCESS_KEY", "", true},
	}
	for _, tc := range cases {
		got, err := srv.resolveBranchesTokenEnv(tc.url, tc.requested)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: err = nil, want rejection", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ── 4. WS origin normalization — wsOriginPatterns ────────────────────────────

func TestWsOriginPatterns(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://example.com", "example.com"},
		{"http://foo.bar:8080", "foo.bar:8080"},
		{"example.com", "example.com"}, // no scheme: pass through unchanged
	}

	for _, tc := range cases {
		got := wsOriginPatterns([]string{tc.input})
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("wsOriginPatterns(%q) = %v, want [%q]", tc.input, got, tc.want)
		}
	}
}

func TestWsOriginPatterns_Multiple(t *testing.T) {
	origins := []string{"https://app.example.com", "http://localhost:3000", "plain.host"}
	got := wsOriginPatterns(origins)
	want := []string{"app.example.com", "localhost:3000", "plain.host"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] got %q, want %q", i, got[i], w)
		}
	}
}

// ── 5. bindAddr helper ────────────────────────────────────────────────────────

func TestBindAddr_AuthFalse_Loopback(t *testing.T) {
	srv, _ := newTestServer(t) // auth is false by default in newTestServer
	addr := srv.bindAddr()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("bindAddr() = %q, want prefix 127.0.0.1:", addr)
	}
}

func TestBindAddr_AuthTrue_AllInterfaces(t *testing.T) {
	srv := newAuthServer(t, "hunter2") // auth: true
	addr := srv.bindAddr()
	if !strings.HasPrefix(addr, ":") {
		t.Errorf("bindAddr() = %q, want prefix :", addr)
	}
	if strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("bindAddr() = %q should not bind loopback when auth is on", addr)
	}
}

// ── 6. secureCookiesFor ─────────────────────────────────────────────────────

func TestSecureCookiesFor(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.ServerConfig
		want bool
	}{
		{"plain http", config.ServerConfig{}, false},
		{"trust proxy", config.ServerConfig{TrustProxy: true}, true},
		{"direct tls", config.ServerConfig{TLSCertFile: "cert.pem", TLSKeyFile: "key.pem"}, true},
		{"tls cert only", config.ServerConfig{TLSCertFile: "cert.pem"}, false},
	}
	for _, tc := range cases {
		got := secureCookiesFor(&config.Config{Server: tc.cfg})
		if got != tc.want {
			t.Errorf("%s: secureCookiesFor = %v, want %v", tc.name, got, tc.want)
		}
	}
	if secureCookiesFor(nil) {
		t.Error("secureCookiesFor(nil) = true, want false")
	}
}
