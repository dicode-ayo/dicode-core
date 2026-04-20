package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// newAuthServer builds a server with auth enabled and a fixed passphrase.
func newAuthServer(t *testing.T, passphrase string) *Server {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:   8080,
			Auth:   true,
			Secret: passphrase,
			MCP:    true,
		},
	}
	srv, err := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// login calls /api/auth/login and returns the session cookie.
func login(t *testing.T, h http.Handler, password string, trust bool) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"password": password, "trust": trust})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

// ── Auth wall ─────────────────────────────────────────────────────────────────

func TestAuth_PublicPathsAlwaysAccessible(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	publicPaths := []string{
		"/api/auth/login",
		"/app/app.js",
		"/sw.js",
	}
	for _, p := range publicPaths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		// Must NOT return 401 (static assets may 404 in tests without embedded FS but never 401).
		if w.Code == http.StatusUnauthorized {
			t.Errorf("public path %s returned 401", p)
		}
	}
}

func TestAuth_ProtectedAPI_Returns401WithoutSession(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	protected := []string{"/api/tasks", "/api/config", "/api/secrets"}
	for _, p := range protected {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s: expected 401, got %d", p, w.Code)
		}
	}
}

func TestAuth_ProtectedAPI_AllowedWithValidSession(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	cookie := login(t, h, "hunter2", false)
	if cookie == nil {
		t.Fatal("login failed — no session cookie")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with valid session, got %d", w.Code)
	}
}

func TestAuth_WrongPassword_Returns401(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{"password": "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong password, got %d", w.Code)
	}
}

func TestAuth_NoAuthConfig_AllEndpointsOpen(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080, Auth: false}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when auth disabled, got %d", w.Code)
	}
}

func TestAuth_Logout_RevokesSession(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	cookie := login(t, h, "hunter2", false)
	if cookie == nil {
		t.Fatal("login failed")
	}

	// Logout.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("logout failed: %d", w.Code)
	}

	// Session should now be invalid.
	req2 := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req2.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 after logout, got %d", w2.Code)
	}
}

func TestAuth_TrustedDevice_IssuedOnLogin(t *testing.T) {
	srv := newAuthServer(t, "secret")
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{"password": "secret", "trust": true})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}

	var foundDevice bool
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookie {
			foundDevice = true
			if c.HttpOnly != true {
				t.Error("device cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Error("device cookie must be SameSite=Strict")
			}
			if c.MaxAge <= 0 {
				t.Error("device cookie MaxAge must be positive")
			}
		}
	}
	if !foundDevice {
		t.Error("expected device cookie to be set when trust=true")
	}
}

func TestAuth_DeviceList_RequiresSession(t *testing.T) {
	srv := newAuthServer(t, "secret")
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/devices", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ── Rate limiter ──────────────────────────────────────────────────────────────

func TestAuth_RateLimit_LoginEndpoint(t *testing.T) {
	srv := newAuthServer(t, "secret")
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{"password": "wrong"})

	var lastCode int
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.1:12345" // same IP every time
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		lastCode = w.Code
	}
	if lastCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 after repeated failures from same IP, got %d", lastCode)
	}
}

// ── CORS ─────────────────────────────────────────────────────────────────────

func TestCORS_DisallowedOrigin_NoHeader(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{
		Port:           8080,
		AllowedOrigins: []string{"https://trusted.example.com"},
	}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no ACAO header for untrusted origin, got %q", got)
	}
}

func TestCORS_AllowedOrigin_HasHeader(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{
		Port:           8080,
		AllowedOrigins: []string{"https://trusted.example.com"},
	}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Origin", "https://trusted.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://trusted.example.com" {
		t.Errorf("expected ACAO=trusted.example.com, got %q", got)
	}
}

// ── Security headers ─────────────────────────────────────────────────────────

func TestSecurityHeaders_Present(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	srv, _ := New(8080, reg, eng, &config.Config{}, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	headers := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for h, want := range headers {
		if got := w.Header().Get(h); got != want {
			t.Errorf("%s: want %q, got %q", h, want, got)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("Content-Security-Policy header missing")
	}
}

// ── API keys ─────────────────────────────────────────────────────────────────

func TestAPIKey_GenerateAndValidate(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	store := newAPIKeyStore(d)
	ctx := t.Context()

	raw, info, err := store.generate(ctx, "test-key")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if info.ID == "" {
		t.Error("expected non-empty ID")
	}
	if len(raw) < 10 || raw[:4] != apiKeyPrefix {
		t.Errorf("raw key format wrong: %q", raw)
	}

	if !store.validate(ctx, raw) {
		t.Error("valid key rejected")
	}
	if store.validate(ctx, "dck_notavalidkey") {
		t.Error("invalid key accepted")
	}
}

func TestAPIKey_Revoke(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	store := newAPIKeyStore(d)
	ctx := t.Context()

	raw, info, err := store.generate(ctx, "to-revoke")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := store.revoke(ctx, info.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if store.validate(ctx, raw) {
		t.Error("revoked key still accepted")
	}
}

func TestAPIKey_MCP_Requires_Key_When_Auth_Enabled(t *testing.T) {
	srv := newAuthServer(t, "secret")
	h := srv.Handler()

	// MCP request without any key.
	body := `{"jsonrpc":"2.0","method":"tools/list","id":1}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for MCP without key, got %d", w.Code)
	}
}

// ── Rate limiter lockout ──────────────────────────────────────────────────────

func TestAuth_RateLimit_ExtendedLockout(t *testing.T) {
	srv := newAuthServer(t, "secret")
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{"password": "wrong"})

	// Exhaust the limit.
	for i := 0; i < unlockMaxAttempts; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.2:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}

	// Next attempt should be 429 immediately (not after a 1-minute reset).
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.2:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 during extended lockout, got %d", w.Code)
	}

	// A different IP must still be allowed.
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.RemoteAddr = "10.0.0.3:1234"
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code == http.StatusTooManyRequests {
		t.Errorf("different IP should not be rate-limited, got %d", w2.Code)
	}
}

// ── X-Forwarded-For trust ─────────────────────────────────────────────────────

func TestClientIP_IgnoresXFFWithoutTrustProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	// trust_proxy: false — must use RemoteAddr, not XFF.
	ip := clientIP(req, false)
	if ip != "10.0.0.1" {
		t.Errorf("expected RemoteAddr IP 10.0.0.1, got %q", ip)
	}
}

func TestClientIP_RespectsXFFWithTrustProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	// trust_proxy: true — must use the leftmost (client) XFF entry.
	ip := clientIP(req, true)
	if ip != "1.2.3.4" {
		t.Errorf("expected XFF IP 1.2.3.4, got %q", ip)
	}
}

// ── CORS origin validation ────────────────────────────────────────────────────

func TestCORS_MalformedOrigin_IsSkipped(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{
		Port: 8080,
		// space-separated string is a common config typo — should be ignored
		AllowedOrigins: []string{"https://good.example.com https://evil.example.com"},
	}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	h := srv.Handler()

	// The malformed entry is skipped, so neither origin gets the CORS header.
	for _, origin := range []string{"https://good.example.com", "https://evil.example.com"} {
		req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("malformed origin entry %q should be skipped, but ACAO=%q for request origin %q", cfg.Server.AllowedOrigins[0], got, origin)
		}
	}
}

// ── Device token rotation ─────────────────────────────────────────────────────

func TestAuth_DeviceToken_Rotation(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer d.Close()

	store := newDBSessionStore(d)
	ctx := t.Context()

	// Issue a device token with a created_at far enough in the past to trigger rotation.
	raw, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	hash := hashToken(raw)
	pastCreated := time.Now().Add(-(deviceRotateAfter + time.Minute)).Unix()
	exp := time.Now().Add(deviceTTL).Unix()
	if err := d.Exec(ctx,
		`INSERT INTO sessions (id, token_hash, kind, label, ip, created_at, last_seen, expires_at)
		 VALUES ('test-id', ?, 'device', 'test', '127.0.0.1', ?, ?, ?)`,
		hash, pastCreated, pastCreated, exp,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	newDevToken, ok := store.renewFromDevice(ctx, raw, "127.0.0.1")
	if !ok {
		t.Fatal("renewFromDevice returned not-ok for valid token")
	}
	if newDevToken == "" {
		t.Error("expected a rotated device token to be returned")
	}
	if newDevToken == raw {
		t.Error("rotated token must differ from the original")
	}

	// Old token must now be rejected.
	_, ok2 := store.renewFromDevice(ctx, raw, "127.0.0.1")
	if ok2 {
		t.Error("old device token should be rejected after rotation")
	}

	// New token must be accepted.
	_, ok3 := store.renewFromDevice(ctx, newDevToken, "127.0.0.1")
	if !ok3 {
		t.Error("new rotated device token should be accepted")
	}
}

// TestWebhookAuthGuard_LongestPrefixWins is a regression test for the auth
// bypass where a public webhook at /hooks/ai would shadow the authenticated
// /hooks/ai/dicodai preset because the guard took the first prefix match from
// a registry sorted by task ID. Under the fix, the guard must pick the
// longest-prefix match — otherwise the `auth: true` override silently drops.
func TestWebhookAuthGuard_LongestPrefixWins(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	// Register two specs with overlapping webhook prefixes. The ID order
	// matters: "ai-agent" sorts before "dicodai" alphabetically, so the
	// bug-producing iteration order is exactly what registry.All()
	// produces on real deployments.
	if err := srv.registry.Register(&task.Spec{
		ID:      "buildin/ai-agent",
		Trigger: task.TriggerConfig{Webhook: "/hooks/ai", WebhookAuth: false},
	}); err != nil {
		t.Fatalf("register ai-agent: %v", err)
	}
	if err := srv.registry.Register(&task.Spec{
		ID:      "buildin/dicodai",
		Trigger: task.TriggerConfig{Webhook: "/hooks/ai/dicodai", WebhookAuth: true},
	}); err != nil {
		t.Fatalf("register dicodai: %v", err)
	}

	h := srv.Handler()

	// Request to the longer, protected path must be rejected without a
	// session even though a shorter, public webhook exists.
	req := httptest.NewRequest(http.MethodPost, "/hooks/ai/dicodai", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /hooks/ai/dicodai without session: expected 401, got %d", w.Code)
	}

	// Request to the shorter, public path must NOT be rejected for lack of
	// a session — the guard's longest-prefix rule must not accidentally
	// flip public webhooks into protected ones either.
	req = httptest.NewRequest(http.MethodPost, "/hooks/ai", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("POST /hooks/ai without session: public webhook should pass through, got 401")
	}
}
