package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// newAuthServerNoDB builds a server with auth enabled but no DB — simulates
// YAML-only passphrase path.
func newAuthServerNoDB(t *testing.T, passphrase string) *Server {
	t.Helper()
	reg := registry.New(nil)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{
		Port:   8080,
		Auth:   true,
		Secret: passphrase,
		MCP:    true,
	}}
	srv := &Server{
		registry: reg,
		engine:   eng,
		cfg:      cfg,
		sessions: newSessionStore(),
		limiter:  newUnlockLimiter(),
		logs:     NewLogBroadcaster(),
		ws:       NewWSHub(zap.NewNop()),
		log:      zap.NewNop(),
		port:     8080,
	}
	return srv
}

// ── passphraseStore unit tests ────────────────────────────────────────────────

func TestPassphraseStore_GetEmpty(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	ps := newPassphraseStore(d)
	val, err := ps.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty passphrase, got %q", val)
	}
}

func TestPassphraseStore_SetAndGet(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	ps := newPassphraseStore(d)
	if err := ps.set(context.Background(), "my-secret-pass"); err != nil {
		t.Fatalf("set: %v", err)
	}
	val, err := ps.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if val != "my-secret-pass" {
		t.Errorf("expected %q, got %q", "my-secret-pass", val)
	}
}

func TestPassphraseStore_Overwrite(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	ps := newPassphraseStore(d)
	_ = ps.set(context.Background(), "first")
	_ = ps.set(context.Background(), "second")

	val, _ := ps.get(context.Background())
	if val != "second" {
		t.Errorf("expected %q after overwrite, got %q", "second", val)
	}
}

// ── resolvePassphrase priority ────────────────────────────────────────────────

func TestResolvePassphrase_YAMLOverrideTakesPrecedence(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	ps := newPassphraseStore(d)
	_ = ps.set(context.Background(), "db-passphrase")

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true, Secret: "yaml-passphrase"}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	got := srv.resolvePassphrase(context.Background())
	if got != "yaml-passphrase" {
		t.Errorf("YAML override should take precedence, got %q", got)
	}
}

func TestResolvePassphrase_DBUsedWhenNoYAML(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true, Secret: ""}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	_ = srv.passphraseStore.set(context.Background(), "stored-pass")

	got := srv.resolvePassphrase(context.Background())
	if got != "stored-pass" {
		t.Errorf("expected DB passphrase, got %q", got)
	}
}

func TestResolvePassphrase_EmptyWhenNeitherSet(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	got := srv.resolvePassphrase(context.Background())
	if got != "" {
		t.Errorf("expected empty passphrase, got %q", got)
	}
}

// ── ensurePassphrase auto-generation ─────────────────────────────────────────

func TestEnsurePassphrase_GeneratesWhenMissing(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	if err := srv.ensurePassphrase(context.Background()); err != nil {
		t.Fatalf("ensurePassphrase: %v", err)
	}

	stored, _ := srv.passphraseStore.get(context.Background())
	if stored == "" {
		t.Error("expected passphrase to be auto-generated and stored")
	}
	if len(stored) < 20 {
		t.Errorf("auto-generated passphrase too short: %q", stored)
	}
}

func TestEnsurePassphrase_DoesNotOverwriteExisting(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	_ = srv.passphraseStore.set(context.Background(), "already-set")
	if err := srv.ensurePassphrase(context.Background()); err != nil {
		t.Fatalf("ensurePassphrase: %v", err)
	}

	stored, _ := srv.passphraseStore.get(context.Background())
	if stored != "already-set" {
		t.Errorf("existing passphrase should not be overwritten, got %q", stored)
	}
}

func TestEnsurePassphrase_NoopWhenAuthDisabled(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: false}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	if err := srv.ensurePassphrase(context.Background()); err != nil {
		t.Fatalf("ensurePassphrase: %v", err)
	}

	stored, _ := srv.passphraseStore.get(context.Background())
	if stored != "" {
		t.Errorf("should not generate passphrase when auth disabled, got %q", stored)
	}
}

func TestEnsurePassphrase_NoopWhenYAMLOverridePresent(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true, Secret: "from-yaml"}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	if err := srv.ensurePassphrase(context.Background()); err != nil {
		t.Fatalf("ensurePassphrase: %v", err)
	}

	stored, _ := srv.passphraseStore.get(context.Background())
	if stored != "" {
		t.Errorf("should not touch DB when YAML override is set, got %q", stored)
	}
}

// ── /api/auth/passphrase HTTP endpoints ───────────────────────────────────────

func TestPassphraseAPI_StatusEndpoint(t *testing.T) {
	srv := newAuthServer(t, "") // auth enabled, no YAML secret
	// Manually set a DB passphrase
	_ = srv.passphraseStore.set(context.Background(), "test-pass")

	h := srv.Handler()
	cookie := login(t, h, "test-pass", false)
	if cookie == nil {
		t.Fatal("login failed")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/passphrase", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["source"] != "db" {
		t.Errorf("expected source=db, got %v", resp)
	}
}

func TestPassphraseAPI_ChangePassphrase(t *testing.T) {
	srv := newAuthServer(t, "")
	_ = srv.passphraseStore.set(context.Background(), "old-passphrase-here")

	h := srv.Handler()
	cookie := login(t, h, "old-passphrase-here", false)
	if cookie == nil {
		t.Fatal("login with old passphrase failed")
	}

	// Change passphrase — must supply the current one.
	body, _ := json.Marshal(map[string]string{"current": "old-passphrase-here", "passphrase": "new-strong-passphrase-123"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on passphrase change, got %d: %s", w.Code, w.Body)
	}

	// Old passphrase must be rejected.
	oldCookie := login(t, h, "old-passphrase-here", false)
	if oldCookie != nil {
		t.Error("old passphrase should no longer work after change")
	}

	// New passphrase must work.
	newCookie := login(t, h, "new-strong-passphrase-123", false)
	if newCookie == nil {
		t.Error("new passphrase should work after change")
	}
}

func TestPassphraseAPI_ChangeRequiresSession(t *testing.T) {
	srv := newAuthServer(t, "")
	_ = srv.passphraseStore.set(context.Background(), "existing-pass-1234")
	h := srv.Handler()

	body, _ := json.Marshal(map[string]string{"current": "existing-pass-1234", "passphrase": "new-pass-should-fail"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No cookie.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without session, got %d", w.Code)
	}
}

func TestPassphraseAPI_ChangeTooShortRejected(t *testing.T) {
	srv := newAuthServer(t, "")
	_ = srv.passphraseStore.set(context.Background(), "existing-long-pass-1234")
	h := srv.Handler()

	cookie := login(t, h, "existing-long-pass-1234", false)
	if cookie == nil {
		t.Fatal("login failed")
	}

	body, _ := json.Marshal(map[string]string{"current": "existing-long-pass-1234", "passphrase": "short"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for short passphrase, got %d", w.Code)
	}
}

func TestPassphraseAPI_WrongCurrentRejected(t *testing.T) {
	srv := newAuthServer(t, "")
	_ = srv.passphraseStore.set(context.Background(), "correct-current-pass-123")
	h := srv.Handler()

	cookie := login(t, h, "correct-current-pass-123", false)
	if cookie == nil {
		t.Fatal("login failed")
	}

	body, _ := json.Marshal(map[string]string{"current": "wrong-current-passphrase", "passphrase": "new-strong-passphrase-123"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong current passphrase, got %d", w.Code)
	}

	// Original passphrase must still work.
	if c := login(t, h, "correct-current-pass-123", false); c == nil {
		t.Error("passphrase should not have changed after rejected attempt")
	}
}

func TestPassphraseAPI_YAMLOverrideBlocksAPIChange(t *testing.T) {
	// When server.secret is set in YAML, the API should refuse to change it
	// to avoid a confusing split-brain state.
	srv := newAuthServer(t, "yaml-controlled-pass")
	h := srv.Handler()

	cookie := login(t, h, "yaml-controlled-pass", false)
	if cookie == nil {
		t.Fatal("login failed")
	}

	body, _ := json.Marshal(map[string]string{"passphrase": "new-pass-via-api-12345"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 when YAML override is active, got %d", w.Code)
	}
}

// ── generateRandomPassphrase ──────────────────────────────────────────────────

func TestGenerateRandomPassphrase_Length(t *testing.T) {
	p, err := generateRandomPassphrase()
	if err != nil {
		t.Fatalf("generateRandomPassphrase: %v", err)
	}
	if len(p) < 20 {
		t.Errorf("passphrase too short: %q (%d chars)", p, len(p))
	}
}

func TestGenerateRandomPassphrase_Unique(t *testing.T) {
	a, _ := generateRandomPassphrase()
	b, _ := generateRandomPassphrase()
	if a == b {
		t.Error("two generated passphrases should not be equal")
	}
}
