package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/db"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
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
// Unscoped (full access) — see generateScoped for the scoped variant used by
// ephemeral per-run MCP tokens.
func (s *apiKeyStore) generate(ctx context.Context, name string) (raw string, info APIKeyInfo, err error) {
	return s.generateScoped(ctx, name, nil)
}

// generateScoped is generate with an optional MCP capability scope. A nil
// scope stores NULL in the scopes column — unscoped/full access, identical
// to generate's current behavior. A non-nil scope (including the zero value
// MCPScope{}, which authorizes nothing) is JSON-marshaled and stored, so a
// caller reading it back via scopeFor gets an enforced restriction rather
// than an accidental full-access key.
func (s *apiKeyStore) generateScoped(ctx context.Context, name string, scope *pkgruntime.MCPScope) (raw string, info APIKeyInfo, err error) {
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

	var scopesJSON any // nil -> SQL NULL
	if scope != nil {
		b, merr := json.Marshal(scope)
		if merr != nil {
			return "", APIKeyInfo{}, fmt.Errorf("marshal mcp scope: %w", merr)
		}
		scopesJSON = string(b)
	}

	if err = s.db.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, prefix, created_at, scopes) VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, hash, prefix, now, scopesJSON,
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

// scopeFor looks up raw's stored MCP scope by hash (same validity rules as
// lookup: must exist and be unexpired). found reports whether the key
// validated at all; scope is nil when found but the key is unscoped (NULL/
// empty scopes column — full access), or non-nil (possibly the zero value,
// which denies everything) when the key carries an explicit scope.
func (s *apiKeyStore) scopeFor(ctx context.Context, raw string) (scope *pkgruntime.MCPScope, found bool) {
	if !strings.HasPrefix(raw, apiKeyPrefix) {
		return nil, false
	}
	hash := hashAPIKey(raw)
	now := time.Now().Unix()

	var id string
	var scopesJSON *string
	_ = s.db.Query(ctx,
		`SELECT id, scopes FROM api_keys WHERE key_hash = ? AND (expires_at IS NULL OR expires_at > ?)`,
		[]any{hash, now},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return rows.Scan(&id, &scopesJSON)
			}
			return nil
		},
	)
	if !found || id == "" {
		return nil, false
	}
	if scopesJSON == nil || *scopesJSON == "" {
		return nil, true
	}
	var s2 pkgruntime.MCPScope
	if err := json.Unmarshal([]byte(*scopesJSON), &s2); err != nil {
		// Corrupt/unrecognized scope data: fail closed (deny everything)
		// rather than silently falling back to unscoped/full access. This
		// should never happen since only generateScoped ever writes this
		// column.
		return &pkgruntime.MCPScope{}, true
	}
	return &s2, true
}

// lookup checks a raw key against stored hashes, updates last_used, and
// returns the key's name when valid and not expired.
func (s *apiKeyStore) lookup(ctx context.Context, raw string) (name string, ok bool) {
	if !strings.HasPrefix(raw, apiKeyPrefix) {
		return "", false
	}
	hash := hashAPIKey(raw)
	now := time.Now().Unix()

	var id string
	_ = s.db.Query(ctx,
		`SELECT id, name FROM api_keys WHERE key_hash = ? AND (expires_at IS NULL OR expires_at > ?)`,
		[]any{hash, now},
		func(rows db.Scanner) error {
			if rows.Next() {
				ok = true
				return rows.Scan(&id, &name)
			}
			return nil
		},
	)
	if ok && id != "" {
		_ = s.db.Exec(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, now, id)
		return name, true
	}
	return "", false
}

// validate reports whether raw is a valid, unexpired API key.
func (s *apiKeyStore) validate(ctx context.Context, raw string) bool {
	_, ok := s.lookup(ctx, raw)
	return ok
}

// validateNonEphemeral is validate that additionally rejects keys minted in
// the ephemeral per-run MCP token namespace. Governance endpoints (approve,
// commit-push) gate on this so a prompt-injected agent holding its own run's
// ephemeral token can't approve or push the very task it just authored —
// which would invert the trust-on-change approval gate.
func (s *apiKeyStore) validateNonEphemeral(ctx context.Context, raw string) bool {
	name, ok := s.lookup(ctx, raw)
	return ok && !strings.HasPrefix(name, ephemeralKeyPrefix)
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

// cliManagedKeyPrefix scopes API-key names that the CLI is allowed to
// revoke-by-name. The slash-separated structure is the marker for
// "tool-managed"; the dashboard's free-form name field can produce
// slashes too, but operators don't typically choose names of this
// shape, so the chance of collision is vanishingly small.
const cliManagedKeyPrefix = "dicode-cli/"

// ephemeralKeyPrefix scopes API-key names minted for ephemeral per-run MCP
// tokens (see mcpTokenMinter) — one key per run, minted at run start and
// revoked when the run ends. Distinct from cliManagedKeyPrefix so the two
// tool-managed namespaces, and operator-created (dashboard) keys, never
// collide.
const ephemeralKeyPrefix = "ephemeral/run/"

// revokeByName deletes API keys with the given exact name. Restricted to
// the CLI-managed (`dicode-cli/...`) and ephemeral per-run MCP token
// (`ephemeral/run/...`) namespaces so a tool-driven revoke can't
// accidentally sweep dashboard-created keys that happen to share a friendly
// name. Returns nil when no rows matched (idempotent).
func (s *apiKeyStore) revokeByName(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, cliManagedKeyPrefix) && !strings.HasPrefix(name, ephemeralKeyPrefix) {
		return fmt.Errorf("revokeByName refused: name %q is not in a tool-managed namespace (%q, %q)", name, cliManagedKeyPrefix, ephemeralKeyPrefix)
	}
	return s.db.Exec(ctx, `DELETE FROM api_keys WHERE name = ?`, name)
}

// revokeByNamePrefix deletes every API key whose name begins with prefix.
// Restricted to the ephemeral per-run MCP token namespace so it can't be
// repurposed to bulk-delete CLI-managed or operator (dashboard) keys. Used
// at daemon startup to sweep tokens orphaned by a run that was in flight
// when the daemon last stopped — nothing will ever call Revoke for it.
// Returns nil when no rows matched (idempotent).
func (s *apiKeyStore) revokeByNamePrefix(ctx context.Context, prefix string) error {
	if prefix != ephemeralKeyPrefix {
		return fmt.Errorf("revokeByNamePrefix refused: prefix %q is not the ephemeral MCP token namespace %q", prefix, ephemeralKeyPrefix)
	}
	return s.db.Exec(ctx, `DELETE FROM api_keys WHERE name LIKE ?`, prefix+"%")
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
	return s.apiKeyMiddleware(s.apiKeys.validate)(next)
}

// requireNonEphemeralAPIKey is requireAPIKey that additionally rejects
// ephemeral per-run MCP tokens, for governance endpoints an agent must not
// reach with its own run's token (see validateNonEphemeral).
func (s *Server) requireNonEphemeralAPIKey(next http.Handler) http.Handler {
	return s.apiKeyMiddleware(s.apiKeys.validateNonEphemeral)(next)
}

func (s *Server) apiKeyMiddleware(validate func(context.Context, string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.cfg.Server.Auth {
				next.ServeHTTP(w, r)
				return
			}
			raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if raw == "" || !validate(r.Context(), raw) {
				s.auditDenied(r, "invalid or missing API key")
				w.Header().Set("WWW-Authenticate", `Bearer realm="dicode"`)
				jsonErr(w, "invalid or missing API key", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
	// The tool-managed namespaces are reserved: a dashboard key named into
	// them would be denied at governance endpoints (validateNonEphemeral) and
	// swept by the startup revoke — surprising data loss. Keep them off-limits
	// to operator-chosen names.
	if strings.HasPrefix(body.Name, ephemeralKeyPrefix) || strings.HasPrefix(body.Name, cliManagedKeyPrefix) {
		jsonErr(w, "name uses a reserved prefix", http.StatusBadRequest)
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
	return s.sm.GetBool(r.Context(), "authenticated")
}
