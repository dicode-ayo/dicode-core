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
// result, returning the hash so callers can warm in-process caches without a
// second DB read. This is the only call sites should use when persisting a
// passphrase from the UI or from auto-generation.
func (p *passphraseStore) setHashed(ctx context.Context, plaintext string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash passphrase: %w", err)
	}
	if err := p.set(ctx, string(hash)); err != nil {
		return "", err
	}
	return string(hash), nil
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

// looksLikeBcryptHash reports whether v has a bcrypt-style prefix. Used to
// distinguish hashed from legacy-plaintext values during the lazy migration
// window. Covers $2a$ (most common), $2b$, $2y$ (PHP variant), and the
// pre-2002 $2$ Blowfish prefix; bcrypt.CompareHashAndPassword accepts all
// of them, so misclassifying any as legacy plaintext would force a needless
// rehash and lock us out for one login on a strict comparator.
func looksLikeBcryptHash(v string) bool {
	return strings.HasPrefix(v, "$2a$") ||
		strings.HasPrefix(v, "$2b$") ||
		strings.HasPrefix(v, "$2y$") ||
		strings.HasPrefix(v, "$2$")
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

	stored, err := s.cachedDBValue(ctx)
	if err != nil {
		s.log.Error("verifyPassphrase: db read failed; failing closed", zap.Error(err))
		return false
	}
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
		if hash, err := s.passphraseStore.setHashed(ctx, candidate); err != nil {
			s.log.Warn("lazy bcrypt migration: failed to rehash legacy passphrase; will retry on next login",
				zap.Error(err))
		} else {
			// Warm the cache with the new hash so subsequent logins skip the
			// legacy branch immediately, without a second DB round-trip.
			s.cachedPassphraseMu.Lock()
			s.cachedPassphrase = hash
			s.cachedPassphraseMu.Unlock()
			s.log.Info("auth passphrase migrated from legacy plaintext to bcrypt hash")
		}
	}
	return true
}

// cachedDBValue returns the DB-stored passphrase value (hash or legacy
// plaintext), reading from cache if warm and falling back to the DB.
//
// Returns ("", nil) when no passphrase is configured (or the store is nil).
// Returns ("", err) when the DB read fails — callers MUST distinguish this
// from "" to avoid fail-open behavior. A swallowed DB error here previously
// allowed apiSecretsUnlock to bypass authentication during a transient DB
// outage; that bug was fixed alongside the bcrypt migration.
func (s *Server) cachedDBValue(ctx context.Context) (string, error) {
	if s.passphraseStore == nil {
		return "", nil
	}
	s.cachedPassphraseMu.RLock()
	cached := s.cachedPassphrase
	s.cachedPassphraseMu.RUnlock()
	if cached != "" {
		return cached, nil
	}
	val, err := s.passphraseStore.get(ctx)
	if err != nil {
		return "", fmt.Errorf("read passphrase from db: %w", err)
	}
	if val != "" {
		s.cachedPassphraseMu.Lock()
		s.cachedPassphrase = val
		s.cachedPassphraseMu.Unlock()
	}
	return val, nil
}

// resolvePassphraseSource describes where the active passphrase comes from.
// It exists so apiGetPassphraseStatus can answer "is one configured?" without
// returning the value itself or accidentally implying plaintext access.
type resolvePassphraseSource string

const (
	passphraseSourceNone resolvePassphraseSource = "none"
	passphraseSourceYAML resolvePassphraseSource = "yaml"
	passphraseSourceDB   resolvePassphraseSource = "db"
	// passphraseSourceUnknown is returned only when the underlying DB read
	// failed; callers must treat it as "passphrase configured but
	// unavailable" so login/secrets-change endpoints fail closed under a
	// transient outage. Distinct from "none" specifically to prevent the
	// bootstrap fast-path (which intentionally accepts any password when
	// no passphrase has been set yet) from being entered on an error.
	passphraseSourceUnknown resolvePassphraseSource = "unknown"
)

// passphraseSource reports where the effective passphrase lives. It does not
// return the value because, post-bcrypt, the DB no longer contains plaintext.
//
// Returns passphraseSourceUnknown if the DB read fails — never silently
// returns passphraseSourceNone on a transient error, which would let
// apiSecretsUnlock skip the verify check and accept any password.
func (s *Server) passphraseSource(ctx context.Context) resolvePassphraseSource {
	if s.cfg.Server.Secret != "" {
		return passphraseSourceYAML
	}
	val, err := s.cachedDBValue(ctx)
	if err != nil {
		s.log.Error("passphraseSource: db read failed; treating as unknown (fail-closed)", zap.Error(err))
		return passphraseSourceUnknown
	}
	if val != "" {
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
	hash, err := s.passphraseStore.setHashed(ctx, plaintext)
	if err != nil {
		return fmt.Errorf("store passphrase: %w", err)
	}
	// Cache the hash so the next login validates against it directly.
	s.cachedPassphraseMu.Lock()
	s.cachedPassphrase = hash
	s.cachedPassphraseMu.Unlock()

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
	// bcrypt silently truncates inputs longer than 72 bytes — reject with a
	// clear error rather than letting the user think the extra characters
	// are protecting them. (UTF-8: len() is byte length, so a few emoji can
	// blow this fast.)
	if len(body.Passphrase) > 72 {
		jsonErr(w, "passphrase must be at most 72 bytes (bcrypt limit)", http.StatusBadRequest)
		return
	}

	// Require the current passphrase to prevent a stolen session from rotating
	// the credential without knowing the existing secret. Skip only when no
	// passphrase is set yet (bootstrap: first-time setup from a valid session).
	// On DB error treat as "configured" — refusing to identify the bootstrap
	// case fails closed and matches apiSecretsUnlock's behavior.
	stored, err := s.cachedDBValue(r.Context())
	if err != nil {
		s.log.Error("apiChangePassphrase: db read failed; failing closed", zap.Error(err))
		jsonErr(w, "service temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if stored != "" {
		if !s.verifyPassphrase(r.Context(), body.Current) {
			jsonErr(w, "current passphrase is incorrect", http.StatusUnauthorized)
			return
		}
	}

	hash, err := s.passphraseStore.setHashed(r.Context(), body.Passphrase)
	if err != nil {
		s.log.Error("failed to store passphrase", zap.Error(err))
		jsonErr(w, "failed to store passphrase", http.StatusInternalServerError)
		return
	}

	// Warm the cache with the new hash directly — saves a DB read on the
	// next verifyPassphrase and keeps observable state consistent with the
	// just-completed write.
	s.cachedPassphraseMu.Lock()
	s.cachedPassphrase = hash
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
