package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

const (
	passphraseKVKey = "auth.passphrase"

	// bcryptCost is the work factor used when hashing a stored passphrase.
	// bcrypt.DefaultCost is 10 (~80ms on a 2024 server CPU). 12 raises that
	// to ~300ms — still tolerable for an interactive login on the rate-limited
	// /api/auth/login endpoint and gives meaningful headroom against offline
	// attacks if the SQLite DB ever leaks.
	bcryptCost = 12
)

// passphraseStore persists the server auth passphrase in the SQLite kv table.
// It is the authoritative source when server.auth is enabled; the YAML
// server.secret field is a fallback override for scripted/headless setups.
//
// The stored value is normally a bcrypt hash (prefix "$2a$"/"$2b$"/"$2y$").
// Older deployments may have a plaintext value left over from before the
// bcrypt migration; verifyPassphrase handles both shapes and rehashes
// plaintext entries on the next successful login.
type passphraseStore struct {
	db db.DB
}

func newPassphraseStore(d db.DB) *passphraseStore { return &passphraseStore{db: d} }

// get returns the raw stored value (hash or legacy plaintext), or "" if none
// is set. Callers must not assume the returned string is plaintext.
func (p *passphraseStore) get(ctx context.Context) (string, error) {
	var val string
	found := false
	err := p.db.Query(ctx,
		`SELECT value FROM kv WHERE key = ?`, []any{passphraseKVKey},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return rows.Scan(&val)
			}
			return nil
		},
	)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	return val, nil
}

// set stores a raw value (hash or, only via legacy migration paths, plaintext)
// replacing any existing value.
func (p *passphraseStore) set(ctx context.Context, value string) error {
	return p.db.Exec(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		passphraseKVKey, value,
	)
}

// setHashed hashes the given plaintext passphrase with bcrypt and stores the
// result. This is the only call sites should use when persisting a
// passphrase from the UI or from auto-generation.
func (p *passphraseStore) setHashed(ctx context.Context, plaintext string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
	if err != nil {
		return fmt.Errorf("hash passphrase: %w", err)
	}
	return p.set(ctx, string(hash))
}

// generateRandomPassphrase produces a cryptographically random passphrase
// (32 bytes, base64url-encoded — 43 printable characters, no padding).
func generateRandomPassphrase() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate passphrase: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// looksLikeBcryptHash reports whether v has one of the bcrypt prefixes
// ($2a$/$2b$/$2y$). Used to distinguish hashed from legacy-plaintext values
// during the lazy migration window.
func looksLikeBcryptHash(v string) bool {
	return strings.HasPrefix(v, "$2a$") || strings.HasPrefix(v, "$2b$") || strings.HasPrefix(v, "$2y$")
}

// verifyPassphrase checks a candidate passphrase against the configured
// authentication source and returns true if it matches. It is the only
// function that should be used to authenticate a user-supplied passphrase.
//
// Resolution order matches resolvePassphrase:
//  1. YAML override (server.secret) — constant-time compare against the
//     plaintext from the config file. We deliberately do not hash YAML
//     overrides: rotating one already requires editing dicode.yaml, and a
//     plaintext-comparable value is what operators expect for headless and
//     scripted setups.
//  2. DB-stored value:
//     - bcrypt hash → bcrypt.CompareHashAndPassword (constant time internally)
//     - legacy plaintext → constant-time compare; on match, opportunistically
//     re-hash and persist so the next login uses bcrypt. Existing
//     deployments therefore migrate transparently on the next successful
//     login without forcing a passphrase reset.
//  3. No passphrase configured anywhere → false.
//
// On any internal error (DB read, rehash failure) the function logs and
// returns false; this fails closed.
func (s *Server) verifyPassphrase(ctx context.Context, candidate string) bool {
	if candidate == "" {
		return false
	}

	// YAML override takes precedence. Compare in constant time.
	if s.cfg.Server.Secret != "" {
		return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.cfg.Server.Secret)) == 1
	}

	stored := s.cachedDBValue(ctx)
	if stored == "" {
		return false
	}

	if looksLikeBcryptHash(stored) {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(candidate)) == nil
	}

	// Legacy plaintext path — constant-time compare, then rehash on success.
	if subtle.ConstantTimeCompare([]byte(candidate), []byte(stored)) != 1 {
		return false
	}
	if s.passphraseStore != nil {
		if err := s.passphraseStore.setHashed(ctx, candidate); err != nil {
			s.log.Warn("lazy bcrypt migration: failed to rehash legacy passphrase; will retry on next login",
				zap.Error(err))
		} else {
			// Refresh the cache to the new hash so subsequent logins skip the
			// legacy branch immediately.
			s.cachedPassphraseMu.Lock()
			s.cachedPassphrase = ""
			s.cachedPassphraseMu.Unlock()
			s.log.Info("auth passphrase migrated from legacy plaintext to bcrypt hash")
		}
	}
	return true
}

// cachedDBValue returns the DB-stored passphrase value (hash or legacy
// plaintext), reading from cache if warm and falling back to the DB. Callers
// must not assume plaintext.
func (s *Server) cachedDBValue(ctx context.Context) string {
	if s.passphraseStore == nil {
		return ""
	}
	s.cachedPassphraseMu.RLock()
	cached := s.cachedPassphrase
	s.cachedPassphraseMu.RUnlock()
	if cached != "" {
		return cached
	}
	val, err := s.passphraseStore.get(ctx)
	if err != nil {
		s.log.Error("failed to read passphrase from DB", zap.Error(err))
		return ""
	}
	if val != "" {
		s.cachedPassphraseMu.Lock()
		s.cachedPassphrase = val
		s.cachedPassphraseMu.Unlock()
	}
	return val
}

// resolvePassphraseSource describes where the active passphrase comes from.
// It exists so apiGetPassphraseStatus can answer "is one configured?" without
// returning the value itself or accidentally implying plaintext access.
type resolvePassphraseSource string

const (
	passphraseSourceNone resolvePassphraseSource = "none"
	passphraseSourceYAML resolvePassphraseSource = "yaml"
	passphraseSourceDB   resolvePassphraseSource = "db"
)

// passphraseSource reports where the effective passphrase lives. It does not
// return the value because, post-bcrypt, the DB no longer contains plaintext.
func (s *Server) passphraseSource(ctx context.Context) resolvePassphraseSource {
	if s.cfg.Server.Secret != "" {
		return passphraseSourceYAML
	}
	if s.cachedDBValue(ctx) != "" {
		return passphraseSourceDB
	}
	return passphraseSourceNone
}

// ensurePassphrase is called at startup when server.auth is true. If no
// passphrase is stored and no YAML override exists, it auto-generates one,
// stores its bcrypt hash, and prints the *plaintext* to stdout so the
// operator can record it. The plaintext only ever lives in this stack
// frame and the operator's terminal scrollback.
func (s *Server) ensurePassphrase(ctx context.Context) error {
	if !s.cfg.Server.Auth {
		return nil // auth disabled — nothing to do
	}
	if s.cfg.Server.Secret != "" {
		return nil // YAML override present — nothing to auto-generate
	}
	if s.passphraseStore == nil {
		return nil
	}
	existing, err := s.passphraseStore.get(ctx)
	if err != nil {
		return fmt.Errorf("read stored passphrase: %w", err)
	}
	if existing != "" {
		// Warm the cache with whatever shape is on disk (hash or legacy
		// plaintext) so the first verifyPassphrase call avoids a DB hit.
		s.cachedPassphraseMu.Lock()
		s.cachedPassphrase = existing
		s.cachedPassphraseMu.Unlock()
		return nil
	}

	// First boot with auth enabled and no passphrase — generate one.
	plaintext, err := generateRandomPassphrase()
	if err != nil {
		return err
	}
	if err := s.passphraseStore.setHashed(ctx, plaintext); err != nil {
		return fmt.Errorf("store passphrase: %w", err)
	}
	// Cache the hash so the next login validates against it directly.
	hash, err := s.passphraseStore.get(ctx)
	if err == nil && hash != "" {
		s.cachedPassphraseMu.Lock()
		s.cachedPassphrase = hash
		s.cachedPassphraseMu.Unlock()
	}

	// Print the plaintext clearly to stdout — this is the operator's only
	// chance to see it. After this function returns, the plaintext is gone.
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  dicode — auth passphrase generated                         ║")
	fmt.Println("║                                                              ║")
	fmt.Printf("║  %s  ║\n", plaintext)
	fmt.Println("║                                                              ║")
	fmt.Println("║  Save this somewhere safe. You can change it any time at    ║")
	fmt.Println("║  /security in the web UI (requires a valid session).        ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	s.log.Info("auth passphrase auto-generated and stored as bcrypt hash")
	return nil
}

// apiGetPassphraseStatus returns whether a passphrase is currently set and
// where it comes from (yaml override, db, or none). Never returns the value.
func (s *Server) apiGetPassphraseStatus(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]string{"source": string(s.passphraseSource(r.Context()))})
}

// apiChangePassphrase allows changing the stored passphrase via the UI.
// Requires the current passphrase to be supplied (or a valid session if
// no passphrase is set yet — bootstrap case).
func (s *Server) apiChangePassphrase(w http.ResponseWriter, r *http.Request) {
	if s.passphraseStore == nil {
		jsonErr(w, "passphrase storage not available", http.StatusServiceUnavailable)
		return
	}
	// Changing the passphrase is a privileged operation — require a valid session.
	if s.cfg.Server.Auth && !s.authSessionValid(r) {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// YAML override is in effect — changing via API would be confusing.
	if s.cfg.Server.Secret != "" {
		jsonErr(w, "passphrase is set via server.secret in dicode.yaml — remove that field to manage it here", http.StatusConflict)
		return
	}

	var body struct {
		Current    string `json:"current"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Passphrase == "" {
		jsonErr(w, "passphrase is required", http.StatusBadRequest)
		return
	}
	if len(body.Passphrase) < 16 {
		jsonErr(w, "passphrase must be at least 16 characters", http.StatusBadRequest)
		return
	}

	// Require the current passphrase to prevent a stolen session from rotating
	// the credential without knowing the existing secret. Skip only when no
	// passphrase is set yet (bootstrap: first-time setup from a valid session).
	if s.cachedDBValue(r.Context()) != "" {
		if !s.verifyPassphrase(r.Context(), body.Current) {
			jsonErr(w, "current passphrase is incorrect", http.StatusUnauthorized)
			return
		}
	}

	if err := s.passphraseStore.setHashed(r.Context(), body.Passphrase); err != nil {
		s.log.Error("failed to store passphrase", zap.Error(err))
		jsonErr(w, "failed to store passphrase", http.StatusInternalServerError)
		return
	}

	// Invalidate the cache so the next verifyPassphrase reloads the new hash.
	s.cachedPassphraseMu.Lock()
	s.cachedPassphrase = ""
	s.cachedPassphraseMu.Unlock()

	// Invalidate all current sessions — everyone must re-login with the new passphrase.
	s.sessions.mu.Lock()
	s.sessions.tokens = make(map[string]time.Time)
	s.sessions.mu.Unlock()
	if s.dbSessions != nil {
		_ = s.dbSessions.revokeAllDevices(r.Context())
	}
	clearAuthCookies(w)

	s.log.Info("auth passphrase changed — all sessions invalidated")
	jsonOK(w, map[string]string{"status": "ok"})
}
