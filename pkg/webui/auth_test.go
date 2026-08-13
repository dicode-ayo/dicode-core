package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	mcpOn := true
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:   8080,
			Auth:   true,
			Secret: passphrase,
			MCP:    &mcpOn,
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

// armSpecWebhook registers an enabled webhook Spec on both the registry and the
// trigger engine, mirroring a live daemon. The engine's webhook dispatch map is
// what the auth guard resolves against, so a registry-only registration would
// not be seen (and would 404 as an inactive path).
func armSpecWebhook(t *testing.T, srv *Server, id, path string, mode task.WebhookAuthMode, secret string) {
	t.Helper()
	s := &task.Spec{
		ID:      id,
		Name:    id,
		Enabled: true,
		Trigger: task.TriggerConfig{Webhook: path, WebhookAuth: mode, WebhookSecret: secret},
	}
	if err := srv.registry.Register(s); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	if err := srv.engine.Register(s); err != nil {
		t.Fatalf("engine register %s: %v", id, err)
	}
}

// armPipelineWebhook registers a minimal valid enabled pipeline (one stage, plus
// its stage task) on both the registry and the engine, so its webhook path lands
// in the engine's dispatch map.
func armPipelineWebhook(t *testing.T, srv *Server, id, path string, mode task.WebhookAuthMode) {
	t.Helper()
	stageID := id + "-stage"
	stage := &task.Spec{ID: stageID, Name: stageID, Enabled: true}
	if err := srv.registry.Register(stage); err != nil {
		t.Fatalf("register stage %s: %v", stageID, err)
	}
	if err := srv.engine.Register(stage); err != nil {
		t.Fatalf("engine register stage %s: %v", stageID, err)
	}
	p := &task.PipelineTask{
		APIVersion: "dicode/v1",
		Kind:       task.KindPipelineTask,
		ID:         id,
		Name:       id,
		Subtype:    "sequential",
		Enabled:    true,
		Trigger:    task.PipelineTrigger{Webhook: path, WebhookAuth: mode},
		Stages:     []task.Stage{{Task: stageID}},
	}
	if err := srv.registry.Register(p); err != nil {
		t.Fatalf("register pipeline %s: %v", id, err)
	}
	if err := srv.engine.Register(p); err != nil {
		t.Fatalf("engine register pipeline %s: %v", id, err)
	}
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

// TestAuth_Logout_RevokesDeviceToken is a regression test for the bug where
// apiLogout passed the raw device-cookie value to the id-keyed revokeDevice,
// matching zero rows and leaving the trusted-device token replayable until its
// 30-day expiry. Logout must delete the row by token_hash so the cookie is
// dead immediately.
func TestAuth_Logout_RevokesDeviceToken(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	// Login with trust=true to get both a session cookie and a device cookie.
	body, _ := json.Marshal(map[string]any{"password": "hunter2", "trust": true})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TestBrowser/1.0")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d: %s", w.Code, w.Body)
	}
	var sessCookie, devCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case sessionCookie:
			sessCookie = c
		case deviceCookie:
			devCookie = c
		}
	}
	if sessCookie == nil || devCookie == nil {
		t.Fatalf("expected session and device cookies, got session=%v device=%v", sessCookie, devCookie)
	}

	devices, err := srv.dbSessions.listDevices(t.Context())
	if err != nil {
		t.Fatalf("listDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device row after trusted login, got %d", len(devices))
	}

	// Logout with the device cookie attached.
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req2.AddCookie(sessCookie)
	req2.AddCookie(devCookie)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("logout failed: %d", w2.Code)
	}

	// The device row must be gone, not just the session.
	devices, err = srv.dbSessions.listDevices(t.Context())
	if err != nil {
		t.Fatalf("listDevices: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("device row must be deleted on logout, got %d rows", len(devices))
	}

	// Replaying the raw device token must fail — the cookie is dead.
	if _, ok, _ := srv.dbSessions.renewFromDevice(t.Context(), devCookie.Value, "127.0.0.1", "TestBrowser/1.0", "off"); ok {
		t.Fatal("device token still accepted after logout; logout must revoke by token_hash")
	}
}

// Guard: the id-keyed revokeDevice used by apiRevokeDevice (revoke a device
// from the /security list) must keep working alongside the token-keyed revoke.
func TestAuth_RevokeDeviceByID_StillWorks(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := t.Context()

	raw, err := store.issueDeviceToken(ctx, "203.0.113.10", "TestBrowser/1.0")
	if err != nil {
		t.Fatalf("issueDeviceToken: %v", err)
	}
	devices, err := store.listDevices(ctx)
	if err != nil {
		t.Fatalf("listDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}

	if err := store.revokeDevice(ctx, devices[0].ID); err != nil {
		t.Fatalf("revokeDevice: %v", err)
	}
	devices, _ = store.listDevices(ctx)
	if len(devices) != 0 {
		t.Fatalf("id-based revoke must delete the row, got %d rows", len(devices))
	}
	if _, ok, _ := store.renewFromDevice(ctx, raw, "203.0.113.10", "TestBrowser/1.0", "off"); ok {
		t.Fatal("device token still accepted after id-based revoke")
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

// ── Rate limiter per-IP isolation ─────────────────────────────────────────────

func TestAuth_RateLimit_DifferentIPNotBlocked(t *testing.T) {
	srv := newAuthServer(t, "secret")
	h := srv.Handler()

	body, _ := json.Marshal(map[string]any{"password": "wrong"})

	// Exhaust the limit for one IP.
	for i := 0; i < unlockMaxAttempts+1; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.2:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
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

	store := newDBSessionStore(d, nil)
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

	newDevToken, ok, _ := store.renewFromDevice(ctx, raw, "127.0.0.1", "", "off")
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
	_, ok2, _ := store.renewFromDevice(ctx, raw, "127.0.0.1", "", "off")
	if ok2 {
		t.Error("old device token should be rejected after rotation")
	}

	// New token must be accepted.
	_, ok3, _ := store.renewFromDevice(ctx, newDevToken, "127.0.0.1", "", "off")
	if !ok3 {
		t.Error("new rotated device token should be accepted")
	}
}

// ── Device-token rotation cookie propagation (#681) ──────────────────────────
//
// Regression coverage for the bug where hasValidSession (used by
// webhookAuthGuard and sessionOrAPIKeyMiddleware) rotated the device row on
// use but silently discarded the freshly-issued token instead of writing it
// back as a cookie — leaving the browser with neither a valid session nor a
// valid device row on its very next request. requireAuth's inline duplicate
// of the rotation logic did write the cookie back, so it was never affected;
// the fix consolidates rotation into hasValidSession so every caller gets it.

// newRotatableDeviceRow inserts a device row old enough (created_at further
// back than deviceRotateAfter) that the next renewFromDevice call rotates it,
// issuing a fresh token. Returns the still-valid raw token to present as the
// device cookie.
func newRotatableDeviceRow(t *testing.T, srv *Server) string {
	t.Helper()
	return issueRow(t, srv.dbSessions.db, "203.0.113.10", strptr(uaFamily(chromeMac)), deviceRotateAfter+time.Hour)
}

// TestAuth_DeviceTokenRotation_WritesCookieBackOnEveryPath is the table-driven
// regression test for #681: a request carrying an expired/absent scs session
// but a valid, renewable device cookie must receive a Set-Cookie for the
// rotated device token no matter which of the three auth gates it passes
// through. Before the fix this failed for the sessionOrAPIKeyMiddleware and
// webhookAuthGuard postures (both go through hasValidSession), while
// requireAuth passed because it duplicated the write-back logic inline.
func TestAuth_DeviceTokenRotation_WritesCookieBackOnEveryPath(t *testing.T) {
	cases := []struct {
		name    string
		request func(t *testing.T, srv *Server) *http.Request
	}{
		{
			// Normal page/API route, gated by requireAuth.
			name: "requireAuth",
			request: func(t *testing.T, srv *Server) *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
				req.Header.Set("User-Agent", chromeMac)
				return req
			},
		},
		{
			// /api/tasks/{id}/approve-style route, gated by
			// sessionOrAPIKeyMiddleware via requireSessionOrNonEphemeralAPIKey.
			name: "sessionOrAPIKeyMiddleware",
			request: func(t *testing.T, srv *Server) *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fsome-task/approve", nil)
				req.Header.Set("User-Agent", chromeMac)
				return req
			},
		},
		{
			// Webhook with trigger.auth: session, gated by webhookAuthGuard.
			name: "webhookAuthGuard session mode",
			request: func(t *testing.T, srv *Server) *http.Request {
				armSpecWebhook(t, srv, "buildin/session-hook", "/hooks/session-hook", task.WebhookAuthSession, "")
				req := httptest.NewRequest(http.MethodPost, "/hooks/session-hook", nil)
				req.Header.Set("User-Agent", chromeMac)
				return req
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAuthServer(t, "hunter2")
			h := srv.Handler()

			raw := newRotatableDeviceRow(t, srv)
			req := tc.request(t, srv)
			req.AddCookie(&http.Cookie{Name: deviceCookie, Value: raw})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			var gotDevCookie *http.Cookie
			for _, c := range w.Result().Cookies() {
				if c.Name == deviceCookie {
					gotDevCookie = c
				}
			}
			if gotDevCookie == nil {
				t.Fatalf("%s: expected a rotated device Set-Cookie in the response, got none (status %d, body %s)", tc.name, w.Code, w.Body)
			}
			if gotDevCookie.Value == "" || gotDevCookie.Value == raw {
				t.Errorf("%s: expected a NEW rotated device token, got %q (original %q)", tc.name, gotDevCookie.Value, raw)
			}
			if w.Code == http.StatusUnauthorized {
				t.Errorf("%s: request should have been let through on a valid renewable device token, got 401", tc.name)
			}
		})
	}
}

// TestAuth_ExpiredSessionLiveDevice_RenewsAndLetsThrough is the specific
// end-to-end case from #681: the scs session cookie is missing/expired but
// the device cookie is present and valid. The request must both receive a
// fresh device Set-Cookie AND be let through (not denied). It drives this
// through sessionOrAPIKeyMiddleware (POST /api/tasks/{id}/approve) — one of
// the two previously-broken paths — and confirms the downstream business
// action (approving a pending task) actually completes, not just that the
// HTTP status looks like a pass: before the fix hasValidSession's ok=true
// already let requests like this one through (so the approval succeeded
// either way), but it silently dropped the rotated cookie, which is the
// specific regression this test locks in.
func TestAuth_ExpiredSessionLiveDevice_RenewsAndLetsThrough(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	h := srv.Handler()

	if err := srv.registry.Register(&task.Spec{ID: "repo/pending-task", Name: "repo/pending-task", Trigger: task.TriggerConfig{Manual: true}, Enabled: true}); err != nil {
		t.Fatalf("register task: %v", err)
	}
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	raw := newRotatableDeviceRow(t, srv)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve", nil)
	req.Header.Set("User-Agent", chromeMac)
	// No session cookie at all — simulates an expired/never-established scs
	// session, exactly as if the cookie had aged out.
	req.AddCookie(&http.Cookie{Name: deviceCookie, Value: raw})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected the request to be let through and the approval to succeed on a valid device token, got %d: %s", w.Code, w.Body)
	}
	if gate.IsPending("repo/pending-task") {
		t.Error("task should have been approved — request was not actually let through")
	}

	var gotDevCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookie {
			gotDevCookie = c
		}
	}
	if gotDevCookie == nil {
		t.Fatal("expected a Set-Cookie for the rotated device token, got none")
	}
	if gotDevCookie.Value == "" || gotDevCookie.Value == raw {
		t.Errorf("expected a NEW rotated device token, got %q (original %q)", gotDevCookie.Value, raw)
	}
	if gotDevCookie.HttpOnly != true {
		t.Error("rotated device cookie must be HttpOnly")
	}

	// The rotated token must now be usable on a follow-up request — proof the
	// old row was replaced, not just duplicated.
	req2 := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req2.Header.Set("User-Agent", chromeMac)
	req2.AddCookie(gotDevCookie)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("rotated device token should be accepted on the next request, got %d: %s", w2.Code, w2.Body)
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
	armSpecWebhook(t, srv, "buildin/ai-agent", "/hooks/ai", task.WebhookAuthNone, "")
	armSpecWebhook(t, srv, "buildin/dicodai", "/hooks/ai/dicodai", task.WebhookAuthSession, "")

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

// TestWebhookAuthGuard_PipelineAuth guards against the auth bypass where a
// kind: PipelineTask webhook with auth: true was silently public because the
// guard only consulted registry.All() (kind: Task). A pipeline declaring
// auth: true must require a session just like a kind: Task webhook.
func TestWebhookAuthGuard_PipelineAuth(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	armPipelineWebhook(t, srv, "deploy-pipe", "/hooks/deploy", task.WebhookAuthSession)
	armPipelineWebhook(t, srv, "public-pipe", "/hooks/public", task.WebhookAuthNone)

	h := srv.Handler()

	// Protected pipeline webhook: rejected without a session.
	req := httptest.NewRequest(http.MethodPost, "/hooks/deploy", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /hooks/deploy (pipeline auth:true) without session: expected 401, got %d", w.Code)
	}

	// Public pipeline webhook: must NOT be turned into a protected one.
	req = httptest.NewRequest(http.MethodPost, "/hooks/public", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("POST /hooks/public (pipeline auth:false): expected pass-through, got 401")
	}
}

// TestWebhookAuthGuard_LongestPrefixWins_Inverse is the symmetric case:
// the SHORT webhook is protected and the LONG one is public. Longest-prefix
// must still win — i.e. a request to the long path must pass through
// unauthenticated because the longer, more specific spec is public. This
// guards against a future regression that accidentally returns the first
// (shorter, protected) match from registry ordering.
func TestWebhookAuthGuard_LongestPrefixWins_Inverse(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	armSpecWebhook(t, srv, "buildin/ai-agent", "/hooks/ai", task.WebhookAuthSession, "")
	armSpecWebhook(t, srv, "buildin/dicodai", "/hooks/ai/dicodai", task.WebhookAuthNone, "")

	h := srv.Handler()

	// Long, public path: pass through without a session.
	req := httptest.NewRequest(http.MethodPost, "/hooks/ai/dicodai", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("POST /hooks/ai/dicodai: longer-public-wins expected, got 401")
	}

	// Short, protected path: rejected without a session.
	req = httptest.NewRequest(http.MethodPost, "/hooks/ai", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /hooks/ai: protected webhook expected 401, got %d", w.Code)
	}
}

// TestWebhookAuthGuard_TrailingSlashPattern covers the exact class of drift
// between the auth guard and the gateway that the shared PathMatches helper
// was introduced to prevent. A webhook registered with a trailing slash
// must still match a longer request path — otherwise `auth: true` would
// silently drop for the misconfigured registration while the gateway
// still dispatched the request.
func TestWebhookAuthGuard_TrailingSlashPattern(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	armSpecWebhook(t, srv, "buildin/trailing", "/hooks/trail/", task.WebhookAuthSession, "")

	h := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/hooks/trail/sub", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("trailing-slash pattern must still gate nested paths: got %d", w.Code)
	}
}

// TestWebhookAuthGuard_RelayRejectsAuthWebhook covers the relay branch: an
// auth:true webhook reached through the relay (trusted X-Relay-Base) can never
// carry a session (the relay strips credentials and only forwards /hooks/*), so
// it is rejected before any session is evaluated — a browser GET gets an HTML
// explainer instead of an unreachable /login bounce, an API call gets JSON. A
// public webhook still passes through, and the direct (non-relay) path still
// redirects to /login.
func TestWebhookAuthGuard_RelayRejectsAuthWebhook(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	armSpecWebhook(t, srv, "buildin/ai-claude", "/hooks/ai-claude", task.WebhookAuthSession, "")
	armSpecWebhook(t, srv, "buildin/open", "/hooks/open", task.WebhookAuthNone, "")

	h := srv.Handler()
	const relayBase = "/u/0000000000000000000000000000000000000000000000000000000000000000"

	// Relayed browser GET → 401 with an HTML explainer, NOT a login redirect.
	req := httptest.NewRequest(http.MethodGet, "/hooks/ai-claude", nil)
	req.Header.Set("X-Relay-Base", relayBase)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("relayed browser GET: expected 401, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("relayed browser GET: expected HTML explainer, got Content-Type %q", ct)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("relayed browser GET must not redirect to login, got Location %q", loc)
	}

	// Relayed API POST (no text/html Accept) → 401 JSON.
	req = httptest.NewRequest(http.MethodPost, "/hooks/ai-claude", nil)
	req.Header.Set("X-Relay-Base", relayBase)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("relayed API POST: expected 401, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("relayed API POST: expected JSON error, got Content-Type %q", ct)
	}

	// Relayed request to a public webhook still passes through (not 401): the
	// relay reject must only fire for auth:true.
	req = httptest.NewRequest(http.MethodPost, "/hooks/open", nil)
	req.Header.Set("X-Relay-Base", relayBase)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("relayed public webhook: expected pass-through, got 401")
	}

	// Direct (no relay header) browser GET still redirects to /login unchanged.
	req = httptest.NewRequest(http.MethodGet, "/hooks/ai-claude", nil)
	req.Header.Set("Accept", "text/html")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Errorf("direct browser GET: expected 303 login redirect, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("direct browser GET: expected /login redirect, got Location %q", loc)
	}
}

// TestResolveWebhookAuth is the pure decision table — no server, no I/O.
func TestResolveWebhookAuth(t *testing.T) {
	const P, G = http.MethodPost, http.MethodGet
	cases := []struct {
		name       string
		mode       task.WebhookAuthMode
		hasSecret  bool
		isAsset    bool
		method     string
		relayed    bool
		hasSession bool
		want       webhookAuthOutcome
	}{
		{"public passes", task.WebhookAuthNone, false, false, P, false, false, webhookPass},
		{"public passes even relayed", task.WebhookAuthNone, false, false, P, true, false, webhookPass},
		// session mode: a session passes WITHOUT the skip flag, so a configured
		// secret still ANDs (the handler verifies the signature).
		{"session+session passes (no skip flag)", task.WebhookAuthSession, false, false, P, false, true, webhookPass},
		{"session+secret+session passes (AND preserved)", task.WebhookAuthSession, true, false, P, false, true, webhookPass},
		{"session no session denies", task.WebhookAuthSession, false, false, P, false, false, webhookDeny},
		{"session relayed denies", task.WebhookAuthSession, true, false, P, true, false, webhookDeny},
		// any mode: a session alone authenticates and skips HMAC+replay.
		{"any+session skips HMAC", task.WebhookAuthAny, true, false, P, false, true, webhookPassSession},
		{"any direct POST no session → HMAC", task.WebhookAuthAny, true, false, P, false, false, webhookHMAC},
		{"any direct GET never falls through", task.WebhookAuthAny, true, false, G, false, false, webhookDeny},
		{"any relayed POST → HMAC", task.WebhookAuthAny, true, false, P, true, false, webhookHMAC},
		{"any relayed GET denies", task.WebhookAuthAny, true, false, G, true, false, webhookDeny},
		{"any without secret denies", task.WebhookAuthAny, false, false, P, false, false, webhookDeny},
		// Asset sub-paths must never fall through to HMAC, even a non-GET one —
		// that would serve auth-gated UI assets to unauthenticated callers.
		{"any asset POST no session denies", task.WebhookAuthAny, true, true, P, false, false, webhookDeny},
		{"any asset POST relayed denies", task.WebhookAuthAny, true, true, P, true, false, webhookDeny},
		{"any asset POST with session skips", task.WebhookAuthAny, true, true, P, false, true, webhookPassSession},
		// Relayed requests must never honour a session, even one that somehow
		// surfaced — HMAC is the only relay credential.
		{"any relayed ignores session", task.WebhookAuthAny, true, false, P, true, true, webhookHMAC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveWebhookAuth(tc.mode, tc.hasSecret, tc.isAsset, tc.method, tc.relayed, tc.hasSession)
			if got != tc.want {
				t.Errorf("resolveWebhookAuth = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestWebhookAuthGuard_AnyMode exercises the guard's decisions for auth: any.
// The guard's only rejection output is a 401/redirect; any other status means
// it delegated the request downstream (to the HMAC-gated handler), which is the
// machine-caller-over-relay path this feature unlocks. The test gateway has no
// route registered, so a delegated request surfaces as 404 — distinct from the
// guard's own 401.
func TestWebhookAuthGuard_AnyMode(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	armSpecWebhook(t, srv, "buildin/ai-claude", "/hooks/ai-claude", task.WebhookAuthAny, "s3cr3t")
	h := srv.Handler()

	// Direct POST, no session → guard delegates to HMAC (not its own 401).
	req := httptest.NewRequest(http.MethodPost, "/hooks/ai-claude", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("any POST no session: guard must delegate to HMAC, got its own 401")
	}

	// Direct GET, no session → guard denies (GET must never fall through to HMAC,
	// or an auth-gated UI would be served). Non-browser GET → 401.
	req = httptest.NewRequest(http.MethodGet, "/hooks/ai-claude", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("any GET no session: expected guard 401, got %d", w.Code)
	}

	// Relayed POST → guard delegates to HMAC (the whole point: a signed machine
	// caller authenticates over the relay).
	req = httptest.NewRequest(http.MethodPost, "/hooks/ai-claude", nil)
	req.Header.Set("X-Relay-Base", "/u/0000000000000000000000000000000000000000000000000000000000000000")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("any relayed POST: guard must delegate to HMAC, got its own 401")
	}

	// Direct POST with a valid session → guard delegates (session path); the
	// downstream skip of signature/replay is covered in the trigger package.
	req = httptest.NewRequest(http.MethodPost, "/hooks/ai-claude", nil)
	req.AddCookie(login(t, h, "hunter2", false))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Errorf("any POST with session: expected delegation, got 401")
	}

	// Unsigned POST to a static-asset sub-path must NOT fall through to HMAC —
	// that would serve the auth-gated UI unauthenticated. The guard denies it
	// (401) rather than delegating.
	req = httptest.NewRequest(http.MethodPost, "/hooks/ai-claude/app.js", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("any unsigned POST to asset sub-path: expected guard 401, got %d", w.Code)
	}
}
