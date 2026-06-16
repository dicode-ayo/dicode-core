package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
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
		"git://github.com/user/repo",
		"ssh://git@github.com/user/repo",
	}
	for _, u := range cases {
		if !isAllowedGitScheme(u) {
			t.Errorf("isAllowedGitScheme(%q) = false, want true", u)
		}
	}
}

func TestIsAllowedGitScheme_Rejected(t *testing.T) {
	cases := []string{
		"file:///etc/passwd",
		"ftp://example.com/repo",
		"data:text/plain,hello",
		"javascript:alert(1)",
		"",
		"not-a-url",
	}
	for _, u := range cases {
		if isAllowedGitScheme(u) {
			t.Errorf("isAllowedGitScheme(%q) = true, want false", u)
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
