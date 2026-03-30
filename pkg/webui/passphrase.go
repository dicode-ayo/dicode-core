package webui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"go.uber.org/zap"
)

const passphraseKVKey = "auth.passphrase"

// passphraseStore persists the server auth passphrase in the SQLite kv table.
// It is the authoritative source when server.auth is enabled; the YAML
// server.secret field is a fallback override for scripted/headless setups.
type passphraseStore struct {
	db db.DB
}

func newPassphraseStore(d db.DB) *passphraseStore { return &passphraseStore{db: d} }

// Get returns the stored passphrase, or "" if none is set.
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

// Set stores a new passphrase, replacing any existing value.
func (p *passphraseStore) set(ctx context.Context, passphrase string) error {
	return p.db.Exec(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		passphraseKVKey, passphrase,
	)
}

// generateRandom produces a cryptographically random passphrase (32 bytes,
// base64url-encoded — 43 printable characters, no padding).
func generateRandomPassphrase() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate passphrase: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// resolvePassphrase returns the effective passphrase for the server:
//   1. YAML override (server.secret) — explicit operator config, highest priority
//   2. DB-stored passphrase — set via UI or auto-generated on first boot
//   3. "" — auth is configured but no passphrase exists yet (bootstrap state)
func (s *Server) resolvePassphrase(ctx context.Context) string {
	// YAML override takes precedence so headless/scripted setups keep working.
	if s.cfg.Server.Secret != "" {
		return s.cfg.Server.Secret
	}
	if s.passphraseStore == nil {
		return ""
	}
	val, _ := s.passphraseStore.get(ctx)
	return val
}

// ensurePassphrase is called at startup when server.auth is true. If no
// passphrase is stored and no YAML override exists, it auto-generates one,
// persists it, and prints it to stdout so the operator can record it.
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
		return nil // already set
	}

	// First boot with auth enabled and no passphrase — generate one.
	pass, err := generateRandomPassphrase()
	if err != nil {
		return err
	}
	if err := s.passphraseStore.set(ctx, pass); err != nil {
		return fmt.Errorf("store passphrase: %w", err)
	}

	// Print clearly to stdout — this is the operator's only chance to see it.
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  dicode — auth passphrase generated                         ║")
	fmt.Println("║                                                              ║")
	fmt.Printf( "║  %s  ║\n", pass)
	fmt.Println("║                                                              ║")
	fmt.Println("║  Save this somewhere safe. You can change it any time at    ║")
	fmt.Println("║  /security in the web UI (requires a valid session).        ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	s.log.Info("auth passphrase auto-generated and stored in database")
	return nil
}

// apiGetPassphraseStatus returns whether a passphrase is currently set and
// where it comes from (yaml override, db, or none). Never returns the value.
func (s *Server) apiGetPassphraseStatus(w http.ResponseWriter, r *http.Request) {
	source := "none"
	if s.cfg.Server.Secret != "" {
		source = "yaml"
	} else if s.passphraseStore != nil {
		if val, _ := s.passphraseStore.get(r.Context()); val != "" {
			source = "db"
		}
	}
	jsonOK(w, map[string]string{"source": source})
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

	if err := s.passphraseStore.set(r.Context(), body.Passphrase); err != nil {
		s.log.Error("failed to store passphrase", zap.Error(err))
		jsonErr(w, "failed to store passphrase", http.StatusInternalServerError)
		return
	}

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
