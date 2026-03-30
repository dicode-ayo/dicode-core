package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const apiKeyPrefix = "dck_"

// apiKeyStore manages API keys stored in SQLite (hashed — raw key shown once).
type apiKeyStore struct {
	db db.DB
}

func newAPIKeyStore(d db.DB) *apiKeyStore { return &apiKeyStore{db: d} }

// Generate creates a new API key, stores its hash, and returns the raw key.
// The raw key is returned only once — callers must present it to the user.
func (s *apiKeyStore) generate(ctx context.Context, name string) (raw string, info APIKeyInfo, err error) {
	rawBytes, err := randomToken()
	if err != nil {
		return "", APIKeyInfo{}, err
	}
	raw = apiKeyPrefix + rawBytes
	hash := hashAPIKey(raw)
	// Show the first 12 chars of the key (prefix + start of random part).
	// The key is always dck_ (4) + 64 hex chars = 68 chars, so this is safe,
	// but guard against any future length change.
	prefixEnd := 12
	if prefixEnd > len(raw) {
		prefixEnd = len(raw)
	}
	prefix := raw[:prefixEnd] + "..."

	id := uuid.New().String()
	now := time.Now().Unix()

	if err = s.db.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, prefix, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, name, hash, prefix, now,
	); err != nil {
		return "", APIKeyInfo{}, err
	}
	info = APIKeyInfo{
		ID:        id,
		Name:      name,
		Prefix:    prefix,
		CreatedAt: time.Unix(now, 0),
	}
	return raw, info, nil
}

// Validate checks a raw key against stored hashes, updates last_used, and
// returns true if valid and not expired.
func (s *apiKeyStore) validate(ctx context.Context, raw string) bool {
	if !strings.HasPrefix(raw, apiKeyPrefix) {
		return false
	}
	hash := hashAPIKey(raw)
	now := time.Now().Unix()
	found := false

	var id string
	_ = s.db.Query(ctx,
		`SELECT id FROM api_keys WHERE key_hash = ? AND (expires_at IS NULL OR expires_at > ?)`,
		[]any{hash, now},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return rows.Scan(&id)
			}
			return nil
		},
	)
	if found && id != "" {
		_ = s.db.Exec(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, now, id)
	}
	return found
}

// List returns all API keys (without hashes).
func (s *apiKeyStore) list(ctx context.Context) ([]APIKeyInfo, error) {
	var keys []APIKeyInfo
	err := s.db.Query(ctx,
		`SELECT id, name, prefix, created_at, last_used, expires_at FROM api_keys ORDER BY created_at DESC`,
		nil,
		func(rows db.Scanner) error {
			for rows.Next() {
				var k APIKeyInfo
				var createdAt int64
				var lastUsed, expiresAt *int64
				if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &createdAt, &lastUsed, &expiresAt); err != nil {
					return err
				}
				k.CreatedAt = time.Unix(createdAt, 0)
				if lastUsed != nil {
					t := time.Unix(*lastUsed, 0)
					k.LastUsed = &t
				}
				if expiresAt != nil {
					t := time.Unix(*expiresAt, 0)
					k.ExpiresAt = &t
				}
				keys = append(keys, k)
			}
			return nil
		},
	)
	return keys, err
}

// Revoke deletes an API key by ID.
func (s *apiKeyStore) revoke(ctx context.Context, id string) error {
	return s.db.Exec(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
}

// APIKeyInfo is the public representation of an API key.
type APIKeyInfo struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// hashAPIKey returns the SHA-256 hex digest of a raw API key.
func hashAPIKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// --- HTTP handlers -----------------------------------------------------------

// requireAPIKey is a middleware that checks for a valid Bearer API key.
// Only active when server.auth is true.
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Server.Auth {
			next.ServeHTTP(w, r)
			return
		}
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" || !s.apiKeys.validate(r.Context(), raw) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dicode"`)
			jsonErr(w, "invalid or missing API key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiListAPIKeys lists all API keys (no raw values).
func (s *Server) apiListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !s.authSessionValid(r) {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	keys, err := s.apiKeys.list(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []APIKeyInfo{}
	}
	jsonOK(w, keys)
}

// apiCreateAPIKey generates a new API key. Returns the raw key once.
func (s *Server) apiCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.authSessionValid(r) {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		jsonErr(w, "name is required", http.StatusBadRequest)
		return
	}
	raw, info, err := s.apiKeys.generate(r.Context(), body.Name)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{
		"key":  raw, // shown once
		"info": info,
	})
}

// apiRevokeAPIKey deletes an API key by ID.
func (s *Server) apiRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.authSessionValid(r) {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.apiKeys.revoke(r.Context(), id); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "revoked"})
}

// authSessionValid is a convenience check used by management endpoints that
// must themselves be behind the session wall (not just API key).
func (s *Server) authSessionValid(r *http.Request) bool {
	if !s.cfg.Server.Auth {
		return true
	}
	c, err := r.Cookie(secretsCookie)
	if err != nil {
		return false
	}
	return s.sessions.valid(c.Value)
}
