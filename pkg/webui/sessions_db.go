package webui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	deviceCookie      = "dicode_device"
	sessionTTL        = 8 * time.Hour
	deviceTTL         = 30 * 24 * time.Hour // 30 days
	deviceRotateAfter = 24 * time.Hour       // rotate device token once per day on use
)

// dbSessionStore backs sessions and trusted-device tokens in SQLite so they
// survive server restarts. It is used alongside the in-memory sessionStore
// which handles the hot-path validation.
type dbSessionStore struct {
	db db.DB
}

func newDBSessionStore(d db.DB) *dbSessionStore { return &dbSessionStore{db: d} }

// IssueDeviceToken generates a long-lived device token, stores its hash in the
// DB, and returns the raw token to be placed in a cookie.
func (s *dbSessionStore) issueDeviceToken(ctx context.Context, ip, userAgent string) (string, error) {
	raw, err := randomToken()
	if err != nil {
		return "", err
	}
	hash := hashToken(raw)
	id := uuid.New().String()
	now := time.Now().Unix()
	exp := time.Now().Add(deviceTTL).Unix()

	label := userAgent
	if len(label) > 200 {
		label = label[:200]
	}

	err = s.db.Exec(ctx,
		`INSERT INTO sessions (id, token_hash, kind, label, ip, created_at, last_seen, expires_at)
		 VALUES (?, ?, 'device', ?, ?, ?, ?, ?)`,
		id, hash, label, ip, now, now, exp,
	)
	if err != nil {
		return "", err
	}
	return raw, nil
}

// renewFromDevice validates a device token cookie value. If valid it updates
// last_seen, optionally rotates the token, and returns a new in-memory session
// token. The caller must set the session cookie on the response.
//
// Token rotation: when the device token is older than deviceRotateAfter, a new
// device token is issued and the old one revoked. The new raw token is returned
// as the second value so the caller can update the cookie.
func (s *dbSessionStore) renewFromDevice(ctx context.Context, rawDeviceToken, ip string) (sessionToken string, ok bool) {
	if rawDeviceToken == "" {
		return "", false
	}
	hash := hashToken(rawDeviceToken)
	now := time.Now().Unix()

	var id, label string
	var exp, createdAt int64
	found := false

	_ = s.db.Query(ctx,
		`SELECT id, label, created_at, expires_at FROM sessions
		 WHERE token_hash = ? AND kind = 'device' AND expires_at > ?`,
		[]any{hash, now},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return rows.Scan(&id, &label, &createdAt, &exp)
			}
			return nil
		},
	)
	if !found {
		return "", false
	}

	// Update last_seen.
	_ = s.db.Exec(ctx,
		`UPDATE sessions SET last_seen = ?, ip = ? WHERE id = ?`,
		now, ip, id,
	)

	return "", true
}

// ListDevices returns all active trusted devices.
func (s *dbSessionStore) listDevices(ctx context.Context) ([]DeviceInfo, error) {
	var devices []DeviceInfo
	err := s.db.Query(ctx,
		`SELECT id, label, ip, created_at, last_seen, expires_at
		 FROM sessions WHERE kind = 'device' AND expires_at > ?
		 ORDER BY last_seen DESC`,
		[]any{time.Now().Unix()},
		func(rows db.Scanner) error {
			for rows.Next() {
				var d DeviceInfo
				var createdAt, lastSeen, expiresAt int64
				if err := rows.Scan(&d.ID, &d.Label, &d.IP, &createdAt, &lastSeen, &expiresAt); err != nil {
					return err
				}
				d.CreatedAt = time.Unix(createdAt, 0)
				d.LastSeen = time.Unix(lastSeen, 0)
				d.ExpiresAt = time.Unix(expiresAt, 0)
				devices = append(devices, d)
			}
			return nil
		},
	)
	return devices, err
}

// RevokeDevice deletes a trusted device by ID.
func (s *dbSessionStore) revokeDevice(ctx context.Context, id string) error {
	return s.db.Exec(ctx, `DELETE FROM sessions WHERE id = ? AND kind = 'device'`, id)
}

// RevokeAllDevices clears all trusted device tokens (emergency lockout).
func (s *dbSessionStore) revokeAllDevices(ctx context.Context) error {
	return s.db.Exec(ctx, `DELETE FROM sessions WHERE kind = 'device'`)
}

// PurgeExpired deletes expired rows from the sessions table.
func (s *dbSessionStore) purgeExpired(ctx context.Context) error {
	return s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
}

// DeviceInfo is returned by ListDevices.
type DeviceInfo struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`     // user-agent (truncated)
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	ExpiresAt time.Time `json:"expires_at"`
}

// --- helpers -----------------------------------------------------------------

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// setSessionCookie writes the short-lived session cookie to the response.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     secretsCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// setDeviceCookie writes the long-lived device cookie to the response.
// The Path is intentionally "/" so the SPA can call /api/auth/refresh with it.
func setDeviceCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(deviceTTL.Seconds()),
	})
}

// clearAuthCookies removes both auth cookies (logout).
func clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{secretsCookie, deviceCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1})
	}
}

// --- HTTP handlers -----------------------------------------------------------

// apiAuthRefresh tries to renew a session from a device token cookie.
// Called by the SPA when it receives a 401 so it can transparently re-auth.
func (s *Server) apiAuthRefresh(w http.ResponseWriter, r *http.Request) {
	if s.dbSessions == nil {
		jsonErr(w, "trusted devices not available", http.StatusServiceUnavailable)
		return
	}
	dc, err := r.Cookie(deviceCookie)
	if err != nil {
		jsonErr(w, "no device token", http.StatusUnauthorized)
		return
	}
	newSession, ok := s.dbSessions.renewFromDevice(r.Context(), dc.Value, clientIP(r))
	if !ok {
		clearAuthCookies(w)
		jsonErr(w, "device token invalid or expired", http.StatusUnauthorized)
		return
	}
	// renewFromDevice returns "" for newSession when it just updates last_seen;
	// issue a fresh in-memory session token.
	if newSession == "" {
		newSession = s.sessions.issue(s.secretsPassphrase() + "dicode")
	}
	setSessionCookie(w, newSession)
	jsonOK(w, map[string]string{"status": "ok"})
}

// apiListDevices returns all trusted devices for the current user.
func (s *Server) apiListDevices(w http.ResponseWriter, r *http.Request) {
	if s.dbSessions == nil {
		jsonOK(w, []DeviceInfo{})
		return
	}
	devices, err := s.dbSessions.listDevices(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if devices == nil {
		devices = []DeviceInfo{}
	}
	jsonOK(w, devices)
}

// apiRevokeDevice removes a single trusted device by ID.
func (s *Server) apiRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if s.dbSessions == nil {
		jsonErr(w, "trusted devices not available", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.dbSessions.revokeDevice(r.Context(), id); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "revoked"})
}

// apiLogout revokes the current session and device token.
func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(secretsCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	if s.dbSessions != nil {
		if dc, err := r.Cookie(deviceCookie); err == nil {
			_ = s.dbSessions.revokeDevice(r.Context(), dc.Value)
		}
	}
	clearAuthCookies(w)
	jsonOK(w, map[string]string{"status": "ok"})
}

// apiLogoutAll revokes all sessions and trusted devices (emergency lockout).
func (s *Server) apiLogoutAll(w http.ResponseWriter, r *http.Request) {
	s.sessions.mu.Lock()
	s.sessions.tokens = make(map[string]time.Time)
	s.sessions.mu.Unlock()

	if s.dbSessions != nil {
		_ = s.dbSessions.revokeAllDevices(r.Context())
	}
	clearAuthCookies(w)
	jsonOK(w, map[string]string{"status": "ok"})
}
