package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

// newAuthServerNoDB builds a server with auth enabled but no DB — simulates
// YAML-only passphrase path.
func newAuthServerNoDB(t *testing.T, passphrase string) *Server {
	t.Helper()
	reg := registry.New(nil)
	eng := trigger.New(reg, nil, zap.NewNop())
	mcpOn := true
	cfg := &config.Config{Server: config.ServerConfig{
		Port:   8080,
		Auth:   true,
		Secret: passphrase,
		MCP:    &mcpOn,
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

func TestPassphraseStore_SetAndGet_RawValue(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	ps := newPassphraseStore(d)
	if err := ps.set(context.Background(), "raw-value"); err != nil {
		t.Fatalf("set: %v", err)
	}
	val, err := ps.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if val != "raw-value" {
		t.Errorf("expected %q, got %q", "raw-value", val)
	}
}

func TestPassphraseStore_SetHashed_StoresBcryptHash(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	ps := newPassphraseStore(d)
	hash, err := ps.setHashed(context.Background(), "my-secret-pass", bcrypt.MinCost)
	if err != nil {
		t.Fatalf("setHashed: %v", err)
	}
	if !looksLikeBcryptHash(hash) {
		t.Fatalf("setHashed must return the bcrypt hash; got %q", hash)
	}
	val, err := ps.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !looksLikeBcryptHash(val) {
		t.Fatalf("stored value should be a bcrypt hash, got %q", val)
	}
	// Plaintext must not appear anywhere in the stored value.
	if strings.Contains(val, "my-secret-pass") {
		t.Errorf("plaintext leaked into stored value: %q", val)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(val), []byte("my-secret-pass")); err != nil {
		t.Errorf("bcrypt compare against stored hash failed: %v", err)
	}
}

func TestPassphraseStore_Overwrite(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	ps := newPassphraseStore(d)
	_, _ = ps.setHashed(context.Background(), "first-pass-1234", bcrypt.MinCost)
	_, _ = ps.setHashed(context.Background(), "second-pass-5678", bcrypt.MinCost)

	val, _ := ps.get(context.Background())
	if !looksLikeBcryptHash(val) {
		t.Fatalf("expected bcrypt hash, got %q", val)
	}
	// Verify the *second* passphrase wins.
	if err := bcrypt.CompareHashAndPassword([]byte(val), []byte("second-pass-5678")); err != nil {
		t.Errorf("expected second hash to verify, got: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(val), []byte("first-pass-1234")); err == nil {
		t.Error("first passphrase should not verify against the overwritten hash")
	}
}

func TestLooksLikeBcryptHash(t *testing.T) {
	cases := map[string]bool{
		// Modern variants — $2a$ is what golang.org/x/crypto/bcrypt emits.
		"$2a$12$abcdef": true,
		"$2b$10$abcdef": true,
		"$2y$12$abcdef": true,
		// Pre-2002 OpenBSD variant. Vanishingly rare in the wild, but
		// bcrypt.CompareHashAndPassword accepts it; misclassifying it as
		// legacy plaintext would force a needless rehash and lock out the
		// account for one login on a stricter comparator.
		"$2$10$abcdef":        true,
		"":                    false,
		"plaintext-pass":      false,
		"$2x$12$weirdvariant": false,
		"prefix$2a$12$x":      false,
	}
	for in, want := range cases {
		if got := looksLikeBcryptHash(in); got != want {
			t.Errorf("looksLikeBcryptHash(%q) = %v, want %v", in, got, want)
		}
	}
}

// ── verifyPassphrase / passphraseSource priority ─────────────────────────────

func TestVerifyPassphrase_YAMLOverrideTakesPrecedence(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true, Secret: "yaml-passphrase"}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	// Even if the DB has a (different) bcrypt hash, the YAML override wins.
	_, _ = srv.passphraseStore.setHashed(context.Background(), "db-passphrase", bcrypt.MinCost)

	if !srv.verifyPassphrase(context.Background(), "yaml-passphrase") {
		t.Error("YAML passphrase should verify when set in config.Server.Secret")
	}
	if srv.verifyPassphrase(context.Background(), "db-passphrase") {
		t.Error("DB passphrase should not verify while YAML override is active")
	}
	if got := srv.passphraseSource(context.Background()); got != passphraseSourceYAML {
		t.Errorf("source = %q, want %q", got, passphraseSourceYAML)
	}
}

func TestVerifyPassphrase_DBHashUsedWhenNoYAML(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true, Secret: ""}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	_, _ = srv.passphraseStore.setHashed(context.Background(), "stored-pass", bcrypt.MinCost)

	if !srv.verifyPassphrase(context.Background(), "stored-pass") {
		t.Error("verifyPassphrase should accept the correct DB-hashed passphrase")
	}
	if srv.verifyPassphrase(context.Background(), "wrong-pass") {
		t.Error("verifyPassphrase should reject a wrong passphrase")
	}
	if got := srv.passphraseSource(context.Background()); got != passphraseSourceDB {
		t.Errorf("source = %q, want %q", got, passphraseSourceDB)
	}
}

func TestVerifyPassphrase_EmptyWhenNeitherSet(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	if srv.verifyPassphrase(context.Background(), "anything") {
		t.Error("verifyPassphrase should reject any candidate when nothing is configured")
	}
	if got := srv.passphraseSource(context.Background()); got != passphraseSourceNone {
		t.Errorf("source = %q, want %q", got, passphraseSourceNone)
	}
}

func TestVerifyPassphrase_RejectsEmptyCandidate(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true, Secret: "yaml"}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	if srv.verifyPassphrase(context.Background(), "") {
		t.Error("empty candidate must never verify")
	}
}

// ── lazy migration: legacy plaintext → bcrypt on next successful login ───────

func TestVerifyPassphrase_LazyMigrationFromLegacyPlaintext(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	// Simulate an existing pre-bcrypt deployment: plaintext value in DB.
	if err := srv.passphraseStore.set(context.Background(), "legacy-plaintext-pass"); err != nil {
		t.Fatalf("set legacy: %v", err)
	}

	// First successful login: must verify AND rehash.
	if !srv.verifyPassphrase(context.Background(), "legacy-plaintext-pass") {
		t.Fatal("legacy plaintext passphrase should verify")
	}

	// DB now contains a bcrypt hash, not plaintext.
	stored, err := srv.passphraseStore.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !looksLikeBcryptHash(stored) {
		t.Fatalf("expected DB to be rehashed to bcrypt, still got %q", stored)
	}
	if strings.Contains(stored, "legacy-plaintext-pass") {
		t.Errorf("plaintext leaked into rehashed value: %q", stored)
	}

	// Subsequent verification must keep working against the new hash.
	if !srv.verifyPassphrase(context.Background(), "legacy-plaintext-pass") {
		t.Error("post-migration verify should still accept the same plaintext")
	}
	if srv.verifyPassphrase(context.Background(), "wrong") {
		t.Error("post-migration verify should reject wrong passphrase")
	}
}

func TestVerifyPassphrase_LegacyMismatchDoesNotMigrate(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	_ = srv.passphraseStore.set(context.Background(), "legacy-plaintext-pass")

	if srv.verifyPassphrase(context.Background(), "wrong-attempt") {
		t.Fatal("wrong passphrase should not verify against legacy plaintext")
	}

	// DB must still be the plaintext — no rehash on a failed attempt.
	stored, _ := srv.passphraseStore.get(context.Background())
	if stored != "legacy-plaintext-pass" {
		t.Errorf("legacy plaintext should be untouched on failed attempt, got %q", stored)
	}
}

// ── ensurePassphrase auto-generation ─────────────────────────────────────────

func TestEnsurePassphrase_GeneratesAndStoresHash(t *testing.T) {
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
		t.Fatal("expected passphrase to be auto-generated and stored")
	}
	// Must be a bcrypt hash, not raw base64 plaintext.
	if !looksLikeBcryptHash(stored) {
		t.Errorf("auto-generated passphrase must be stored as bcrypt hash, got %q", stored)
	}
}

func TestEnsurePassphrase_DoesNotOverwriteExisting(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	_, _ = srv.passphraseStore.setHashed(context.Background(), "already-set-1234", bcrypt.MinCost)
	before, _ := srv.passphraseStore.get(context.Background())

	if err := srv.ensurePassphrase(context.Background()); err != nil {
		t.Fatalf("ensurePassphrase: %v", err)
	}

	after, _ := srv.passphraseStore.get(context.Background())
	if after != before {
		t.Errorf("existing passphrase hash should not be overwritten\n before: %q\n after:  %q", before, after)
	}
}

func TestEnsurePassphrase_DoesNotOverwriteLegacyPlaintext(t *testing.T) {
	// If the DB already holds a legacy plaintext value, ensurePassphrase
	// must leave it alone — the migration happens lazily on the next login.
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	_ = srv.passphraseStore.set(context.Background(), "legacy-plaintext-pass")

	if err := srv.ensurePassphrase(context.Background()); err != nil {
		t.Fatalf("ensurePassphrase: %v", err)
	}
	after, _ := srv.passphraseStore.get(context.Background())
	if after != "legacy-plaintext-pass" {
		t.Errorf("legacy plaintext should be left intact for lazy migration, got %q", after)
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
	// Manually set a DB passphrase via the hashed path.
	_, _ = srv.passphraseStore.setHashed(context.Background(), "test-pass-1234", bcrypt.MinCost)

	h := srv.Handler()
	cookie := login(t, h, "test-pass-1234", false)
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
	_, _ = srv.passphraseStore.setHashed(context.Background(), "old-passphrase-here", bcrypt.MinCost)

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

	// New value must be stored as a bcrypt hash, not plaintext.
	stored, _ := srv.passphraseStore.get(context.Background())
	if !looksLikeBcryptHash(stored) {
		t.Errorf("expected new passphrase to be stored as bcrypt hash, got %q", stored)
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

func TestPassphraseAPI_ChangeFromLegacyPlaintext(t *testing.T) {
	// An operator on a pre-bcrypt deployment can rotate via the API; the
	// "current" they supply is plaintext, which must verify against the
	// legacy plaintext DB value, and the new value must land as a bcrypt hash.
	srv := newAuthServer(t, "")
	_ = srv.passphraseStore.set(context.Background(), "legacy-plain-1234")

	h := srv.Handler()
	cookie := login(t, h, "legacy-plain-1234", false)
	if cookie == nil {
		t.Fatal("legacy login should still work pre-rotation")
	}

	body, _ := json.Marshal(map[string]string{"current": "legacy-plain-1234", "passphrase": "rotated-passphrase-99"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on rotation from legacy plaintext, got %d: %s", w.Code, w.Body)
	}

	stored, _ := srv.passphraseStore.get(context.Background())
	if !looksLikeBcryptHash(stored) {
		t.Errorf("rotated value should be a bcrypt hash, got %q", stored)
	}
	if login(t, h, "rotated-passphrase-99", false) == nil {
		t.Error("rotated passphrase should authenticate")
	}
}

func TestPassphraseAPI_ChangeRequiresSession(t *testing.T) {
	srv := newAuthServer(t, "")
	_, _ = srv.passphraseStore.setHashed(context.Background(), "existing-pass-1234", bcrypt.MinCost)
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
	_, _ = srv.passphraseStore.setHashed(context.Background(), "existing-long-pass-1234", bcrypt.MinCost)
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
	_, _ = srv.passphraseStore.setHashed(context.Background(), "correct-current-pass-123", bcrypt.MinCost)
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

// ── DB-error fail-closed paths ────────────────────────────────────────────────
//
// failingDB wraps a real DB but rejects every Query — used to drive the
// "passphrase row read failed transiently" branch on the verify, source, and
// change paths. We don't pass nil for a missing method because passphraseStore
// only ever calls Query / Exec.
type failingDB struct {
	db.DB
	queryErr error
	execErr  error
}

func (f *failingDB) Query(_ context.Context, _ string, _ []any, _ func(db.Scanner) error) error {
	return f.queryErr
}

func (f *failingDB) Exec(_ context.Context, _ string, _ ...any) error {
	return f.execErr
}

// invalidateCache clears the in-process passphrase cache so the next read goes
// through to the (failing) DB.
func invalidateCache(s *Server) {
	s.cachedPassphraseMu.Lock()
	s.cachedPassphrase = ""
	s.cachedPassphraseMu.Unlock()
}

func TestVerifyPassphrase_DBErrorFailsClosed(t *testing.T) {
	srv := newAuthServer(t, "")
	_, _ = srv.passphraseStore.setHashed(context.Background(), "stored-pass-1234", bcrypt.MinCost)

	srv.passphraseStore = newPassphraseStore(&failingDB{queryErr: context.DeadlineExceeded})
	invalidateCache(srv)

	if srv.verifyPassphrase(context.Background(), "stored-pass-1234") {
		t.Fatal("verifyPassphrase must fail closed when the DB read errors")
	}
}

func TestPassphraseSource_DBErrorReturnsUnknown(t *testing.T) {
	srv := newAuthServer(t, "")
	_, _ = srv.passphraseStore.setHashed(context.Background(), "stored-pass-1234", bcrypt.MinCost)

	srv.passphraseStore = newPassphraseStore(&failingDB{queryErr: context.DeadlineExceeded})
	invalidateCache(srv)

	if got := srv.passphraseSource(context.Background()); got != passphraseSourceUnknown {
		t.Errorf("passphraseSource on db error = %q; want %q", got, passphraseSourceUnknown)
	}
}

// TestApiSecretsUnlock_DBErrorRejects exercises the regression that originally
// motivated this whole patch: a transient DB outage previously made
// `passphraseSource` return `none`, and `apiSecretsUnlock`'s bootstrap
// fast-path then accepted ANY password. Now it must reject with 503.
func TestApiSecretsUnlock_DBErrorRejects(t *testing.T) {
	srv := newAuthServer(t, "")
	_, _ = srv.passphraseStore.setHashed(context.Background(), "stored-pass-1234", bcrypt.MinCost)

	srv.passphraseStore = newPassphraseStore(&failingDB{queryErr: context.DeadlineExceeded})
	invalidateCache(srv)

	h := srv.Handler()
	body, _ := json.Marshal(map[string]any{"password": "anything-at-all"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on DB outage, got %d (body: %s)", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Errorf("must not issue a session cookie under db outage; got %q", c.Value)
		}
	}
}

func TestApiChangePassphrase_DBErrorRejects(t *testing.T) {
	srv := newAuthServer(t, "")
	_, _ = srv.passphraseStore.setHashed(context.Background(), "old-pass-here-123", bcrypt.MinCost)

	h := srv.Handler()
	cookie := login(t, h, "old-pass-here-123", false)
	if cookie == nil {
		t.Fatal("pre-outage login should succeed")
	}

	srv.passphraseStore = newPassphraseStore(&failingDB{queryErr: context.DeadlineExceeded})
	invalidateCache(srv)

	body, _ := json.Marshal(map[string]string{
		"current":    "old-pass-here-123",
		"passphrase": "new-pass-here-1234567",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on DB outage, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// ── concurrent lazy migration (#209) ─────────────────────────────────────────
//
// Two simultaneous successful logins on a legacy plaintext passphrase must
// produce exactly one bcrypt computation and one DB write. Without the
// singleflight-backed migrator a race could land two writes (idempotent on
// the row, but doubling bcrypt CPU and emitting two "migrated" audit-log
// entries when one is correct).

// countingDB wraps a real DB and tallies how many times the passphrase row
// was written. We only care about Exec calls that include the passphraseKVKey
// — there are other kv writes in the broader test setup (and we don't want
// to count the initial seed write the test itself does).
type countingDB struct {
	db.DB
	mu        sync.Mutex
	writes    int
	countOnly string // only count Exec calls whose args include this string
}

func (c *countingDB) Exec(ctx context.Context, query string, args ...any) error {
	if c.countOnly != "" {
		for _, a := range args {
			if s, ok := a.(string); ok && s == c.countOnly {
				c.mu.Lock()
				c.writes++
				c.mu.Unlock()
				break
			}
		}
	}
	return c.DB.Exec(ctx, query, args...)
}

func (c *countingDB) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func TestVerifyPassphrase_ConcurrentLegacyMigration_SingleWrite(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Auth: true, BcryptCost: bcrypt.MinCost}}
	srv, _ := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())

	// Seed the legacy plaintext value via the underlying DB directly so the
	// counting wrapper doesn't see this write.
	if err := srv.passphraseStore.set(context.Background(), "legacy-plain-pass-pp"); err != nil {
		t.Fatal(err)
	}

	// Now swap the store's DB for a counting wrapper. The migrator's writes
	// (via setHashed → Exec INSERT…ON CONFLICT) will be counted.
	cdb := &countingDB{DB: d, countOnly: passphraseKVKey}
	srv.passphraseStore = newPassphraseStore(cdb)

	// Race N goroutines on the same valid legacy plaintext.
	const N = 16
	var (
		wg     sync.WaitGroup
		ready  sync.WaitGroup
		start  = make(chan struct{})
		oks    int32
	)
	wg.Add(N)
	ready.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ready.Done()
			<-start // line everyone up at the same instant
			if srv.verifyPassphrase(context.Background(), "legacy-plain-pass-pp") {
				atomic.AddInt32(&oks, 1)
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()

	if oks != N {
		t.Fatalf("all %d concurrent verifications should succeed, got %d", N, oks)
	}

	// The whole point of the singleflight migrator: at most ONE bcrypt+write
	// for N concurrent valid logins. We allow 1 here. Allowing 0 would be
	// wrong — exactly one goroutine must have done the migration. >1 means
	// the race is unfixed.
	if got := cdb.writeCount(); got != 1 {
		t.Errorf("expected exactly 1 migration write under N=%d concurrent legacy logins, got %d", N, got)
	}

	// And the post-race state must be a bcrypt hash, not the legacy plaintext.
	stored, _ := srv.passphraseStore.get(context.Background())
	if !looksLikeBcryptHash(stored) {
		t.Errorf("expected DB to be migrated to bcrypt, got %q", stored)
	}
}

// ── configurable bcrypt cost (#209) ──────────────────────────────────────────

// The server.bcrypt_cost YAML knob must reach GenerateFromPassword at every
// hashing site (apiChangePassphrase, ensurePassphrase, lazy migration). We
// inspect the resulting hash via bcrypt.Cost — it returns the cost embedded
// in the hash header — to assert the wire-up rather than mocking.
func TestApiChangePassphrase_HonorsConfiguredBcryptCost(t *testing.T) {
	srv := newAuthServer(t, "")
	srv.cfg.Server.BcryptCost = 5 // intentionally non-default; still > MinCost
	_, _ = srv.passphraseStore.setHashed(context.Background(), "old-pass-here-123", bcrypt.MinCost)

	h := srv.Handler()
	cookie := login(t, h, "old-pass-here-123", false)
	if cookie == nil {
		t.Fatal("login failed")
	}

	body, _ := json.Marshal(map[string]string{
		"current":    "old-pass-here-123",
		"passphrase": "new-strong-passphrase-123",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	stored, _ := srv.passphraseStore.get(context.Background())
	cost, err := bcrypt.Cost([]byte(stored))
	if err != nil {
		t.Fatalf("bcrypt.Cost: %v", err)
	}
	if cost != 5 {
		t.Errorf("stored hash cost = %d, want 5 (configured)", cost)
	}
}

// setHashed must treat cost <= 0 as "use the package default" — protects
// against test helpers and hand-built Config structs that skip applyDefaults.
// Without this we'd silently pass 0 to bcrypt, which falls back to its own
// DefaultCost (10) — close to what we want, but we prefer the documented
// constant we control.
func TestSetHashed_ZeroCostFallsBackToDefault(t *testing.T) {
	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()

	ps := newPassphraseStore(d)
	hash, err := ps.setHashed(context.Background(), "pp-default-cost", 0)
	if err != nil {
		t.Fatalf("setHashed: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("bcrypt.Cost: %v", err)
	}
	if cost != defaultBcryptCost {
		t.Errorf("hash cost = %d, want %d (defaultBcryptCost)", cost, defaultBcryptCost)
	}
}

// ── bcrypt input length cap ──────────────────────────────────────────────────

func TestPassphraseAPI_ChangeRejectsLongerThanBcryptLimit(t *testing.T) {
	srv := newAuthServer(t, "")
	_, _ = srv.passphraseStore.setHashed(context.Background(), "current-pass-1234567", bcrypt.MinCost)

	h := srv.Handler()
	cookie := login(t, h, "current-pass-1234567", false)
	if cookie == nil {
		t.Fatal("login failed")
	}

	// 73 bytes — one over bcrypt's silent-truncation threshold.
	tooLong := strings.Repeat("a", 73)
	body, _ := json.Marshal(map[string]string{
		"current":    "current-pass-1234567",
		"passphrase": tooLong,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passphrase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("73-byte passphrase should be rejected with 400, got %d (body: %s)", w.Code, w.Body.String())
	}
}
