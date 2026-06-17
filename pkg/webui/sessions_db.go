package webui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/netip"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	deviceCookie      = "dicode_device"
	sessionTTL        = 8 * time.Hour
	deviceTTL         = 30 * 24 * time.Hour // 30 days
	deviceRotateAfter = 24 * time.Hour      // rotate device token once per day on use
)

// dbSessionStore backs sessions and trusted-device tokens in SQLite so they
// survive server restarts. Short-lived browser sessions are managed by the
// scs.SessionManager; this store handles only device tokens.
type dbSessionStore struct {
	db  db.DB
	log *zap.Logger
}

func newDBSessionStore(d db.DB, log *zap.Logger) *dbSessionStore {
	if log == nil {
		log = zap.NewNop()
	}
	return &dbSessionStore{db: d, log: log}
}

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
	fam := uaFamily(userAgent)

	err = s.db.Exec(ctx,
		`INSERT INTO sessions (id, token_hash, kind, label, ip, ua_family, created_at, last_seen, expires_at)
		 VALUES (?, ?, 'device', ?, ?, ?, ?, ?, ?)`,
		id, hash, label, ip, fam, now, now, exp,
	)
	if err != nil {
		return "", err
	}
	return raw, nil
}

// Device-binding modes for renewFromDevice. Mirror the validated
// server.device_binding config values.
const (
	bindingOff    = "off"
	bindingWarn   = "warn"
	bindingStrict = "strict"
)

// renewFromDevice validates a device token cookie value. If valid it updates
// last_seen inside a transaction and returns ok=true. When the token is older
// than deviceRotateAfter a new device token is issued and the old one deleted
// atomically; the new raw token is returned in newDeviceToken so the caller
// can set a fresh device cookie. The caller is responsible for establishing
// a new scs session.
//
// mode controls IP-subnet / UA-family binding enforcement (see the package's
// device_binding config). In strict mode a /24 (IPv4) or /48 (IPv6) subnet
// change or a UA-family mismatch rejects the renewal (ok=false) AND hard-revokes
// the device row in the same transaction, so a cookie judged stolen cannot be
// replayed until its 30-day expiry; driftReject is set so the caller clears the
// device cookie. In warn mode the renewal proceeds but the drift is recorded on
// the row so /security can surface it; warn-mode drift is sticky — the stored
// baseline IP/ua_family is not re-anchored to the drifted values while a drift
// is flagged, so a persistent drift keeps showing until the client genuinely
// returns to its issuing subnet/family. A stored ua_family of NULL (rows issued
// before this feature) is never treated as a mismatch; the current family is
// recorded on renewal.
func (s *dbSessionStore) renewFromDevice(ctx context.Context, rawDeviceToken, ip, userAgent, mode string) (newDeviceToken string, ok bool, driftReject bool) {
	if rawDeviceToken == "" {
		return "", false, false
	}
	hash := hashToken(rawDeviceToken)
	now := time.Now()
	nowUnix := now.Unix()
	curFam := uaFamily(userAgent)

	var notFound, rejected bool
	var rotated string
	var driftReason, driftDevice, driftStoredIP string

	err := s.db.Tx(ctx, func(tx db.DB) error {
		var id, label, storedIP, storedReason string
		var storedFam *string
		var createdAt int64
		found := false

		if err := tx.Query(ctx,
			`SELECT id, label, ip, ua_family, drift_reason, created_at FROM sessions
			 WHERE token_hash = ? AND kind = 'device' AND expires_at > ?`,
			[]any{hash, nowUnix},
			func(rows db.Scanner) error {
				if rows.Next() {
					found = true
					return rows.Scan(&id, &label, &storedIP, &storedFam, &storedReason, &createdAt)
				}
				return nil
			},
		); err != nil {
			return err
		}
		if !found {
			notFound = true
			return nil
		}

		drift, reason := deviceDrift(storedIP, ip, storedFam, curFam, mode == bindingStrict)
		if drift && mode == bindingStrict {
			// Hard-revoke: a strict-mode drift means the cookie is presenting
			// from an unexpected subnet/UA, so delete the row in this same
			// transaction rather than leaving it replayable until expiry.
			if err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
				return err
			}
			rejected = true
			return nil
		}
		// warn mode records the drift on the row and logs the event after commit.
		// Only persist a reason when actually drifting (vs the issue-time anchor)
		// so a genuine return to baseline clears the flag.
		persistReason := ""
		if drift && mode == bindingWarn {
			driftReason, driftDevice, driftStoredIP = reason, id, storedIP
			persistReason = reason
		}

		// Anchor the stored baseline IP/ua_family. In warn mode, while the device
		// has ever drifted (or is drifting now) keep the baseline pinned to the
		// issuing values instead of re-baselining to the drifted client; the
		// drift comparison above is against storedIP/storedFam, so re-baselining
		// would let a persistent drift self-heal after a single renewal. Off mode
		// and never-drifted devices track the live client as before.
		anchorIP, anchorFam := ip, curFam
		if mode == bindingWarn && (drift || storedReason != "") {
			anchorIP = storedIP
			if storedFam != nil {
				anchorFam = *storedFam
			}
			// storedFam == nil (legacy NULL): backfill with curFam (anchorFam
			// already holds it) so the baseline gains a UA family exactly once.
		}

		age := now.Sub(time.Unix(createdAt, 0))
		if age >= deviceRotateAfter {
			// Rotate: insert a fresh token, delete the old one. The fresh row
			// carries the anchored IP/UA family so warn-mode drift stays sticky
			// across a rotation rather than re-baselining to the drifted client.
			raw, err := randomToken()
			if err != nil {
				return err
			}
			newHash := hashToken(raw)
			newExp := now.Add(deviceTTL).Unix()
			if err := tx.Exec(ctx,
				`INSERT INTO sessions (id, token_hash, kind, label, ip, ua_family, drift_reason, created_at, last_seen, expires_at)
				 VALUES (?, ?, 'device', ?, ?, ?, ?, ?, ?, ?)`,
				uuid.New().String(), newHash, label, anchorIP, anchorFam, persistReason, nowUnix, nowUnix, newExp,
			); err != nil {
				return err
			}
			if err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
				return err
			}
			rotated = raw
		} else {
			// Below the rotation window the token is unchanged; refresh ip and
			// backfill ua_family (NULL on legacy rows) so the next comparison
			// has a baseline.
			if err := tx.Exec(ctx,
				`UPDATE sessions SET last_seen = ?, ip = ?, ua_family = ?, drift_reason = ? WHERE id = ?`,
				nowUnix, anchorIP, anchorFam, persistReason, id,
			); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil || notFound {
		return "", false, false
	}
	if rejected {
		return "", false, true
	}
	if driftReason != "" {
		// Structured device.binding_drift event. Drift is also recorded on the
		// row (drift_reason) and surfaced inline on /security.
		s.log.Warn("device.binding_drift",
			zap.String("event", "device.binding_drift"),
			zap.String("device_id", driftDevice),
			zap.String("reason", driftReason),
			zap.String("stored_ip", driftStoredIP),
			zap.String("current_ip", ip),
		)
	}
	return rotated, true, false // rotated is "" when no rotation occurred
}

// deviceDrift reports whether the presenting IP subnet or UA family differs
// from what was recorded for a device token. A stored UA family of NULL (legacy
// rows) is not a mismatch. The reason string ("ip", "ua", "ip+ua", or "") is
// suitable for surfacing on /security.
//
// strict makes the binding fail closed when the device had a recorded signal but
// the request carries none: an empty curIP against a recorded storedIP, or an
// empty curFam against a recorded storedFam, both count as drift. This blocks a
// crafted request that strips the IP/UA signal (e.g. an X-Forwarded-For token
// that normalizes to "" or a missing User-Agent header) from silently disabling
// a binding axis. In warn/off the empty-signal case is not flagged so a transient
// missing signal does not raise a benign warning. Legacy rows with no recorded
// ua_family (storedFam == nil) never drift on the UA axis regardless of mode.
func deviceDrift(storedIP, curIP string, storedFam *string, curFam string, strict bool) (bool, string) {
	ipDrift := storedIP != "" && curIP != "" && !sameSubnet(storedIP, curIP)
	uaDrift := storedFam != nil && *storedFam != "" && curFam != "" && *storedFam != curFam
	if strict {
		// Fail closed: a recorded signal with no incoming counterpart is drift,
		// not "no signal". curFam is only enforced when the row has a recorded
		// family, so legacy NULL rows still do not drift here.
		if storedIP != "" && curIP == "" {
			ipDrift = true
		}
		if storedFam != nil && *storedFam != "" && curFam == "" {
			uaDrift = true
		}
	}

	switch {
	case ipDrift && uaDrift:
		return true, "ip+ua"
	case ipDrift:
		return true, "ip"
	case uaDrift:
		return true, "ua"
	default:
		return false, ""
	}
}

// sameSubnet reports whether two IPs share a subnet at the binding granularity:
// /24 for IPv4, /48 for IPv6. Coarse on purpose so mobile NAT and carrier IP
// churn within a network do not trip the binding. Unparseable inputs fall back
// to exact-string equality.
func sameSubnet(a, b string) bool {
	ipa, erra := netip.ParseAddr(a)
	ipb, errb := netip.ParseAddr(b)
	if erra != nil || errb != nil {
		return a == b
	}
	// Unmap IPv4-in-IPv6 (::ffff:a.b.c.d) so a mapped and a native form of the
	// same IPv4 address compare in the same family at /24.
	ipa = ipa.Unmap()
	ipb = ipb.Unmap()
	if ipa.Is4() != ipb.Is4() {
		return false
	}
	bits := 24
	if ipa.Is6() {
		bits = 48
	}
	pa, err := ipa.Prefix(bits)
	if err != nil {
		return false
	}
	return pa.Contains(ipb)
}

// ListDevices returns all active trusted devices.
func (s *dbSessionStore) listDevices(ctx context.Context) ([]DeviceInfo, error) {
	var devices []DeviceInfo
	err := s.db.Query(ctx,
		`SELECT id, label, ip, drift_reason, created_at, last_seen, expires_at
		 FROM sessions WHERE kind = 'device' AND expires_at > ?
		 ORDER BY last_seen DESC`,
		[]any{time.Now().Unix()},
		func(rows db.Scanner) error {
			for rows.Next() {
				var d DeviceInfo
				var createdAt, lastSeen, expiresAt int64
				if err := rows.Scan(&d.ID, &d.Label, &d.IP, &d.DriftReason, &createdAt, &lastSeen, &expiresAt); err != nil {
					return err
				}
				d.Drift = d.DriftReason != ""
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
	Label     string    `json:"label"` // user-agent (truncated)
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	ExpiresAt time.Time `json:"expires_at"`
	// Drift is true when the last renewal under warn-mode binding came from a
	// different IP subnet or UA family than the device was issued for.
	Drift       bool   `json:"drift"`
	DriftReason string `json:"drift_reason,omitempty"` // "ip", "ua", or "ip+ua"
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

// setDeviceCookie writes the long-lived device cookie to the response.
// The Path is "/" so the SPA can call /api/auth/refresh with it.
// secure should be true when the connection is HTTPS (see secureCookies) so
// the Secure flag is set on the cookie.
func setDeviceCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(deviceTTL.Seconds()),
		Secure:   secure,
	})
}

// clearDeviceCookie removes the device cookie. The session cookie is managed
// by scs (via sm.Destroy) and does not need manual clearing.
func clearDeviceCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: deviceCookie, Path: "/", MaxAge: -1})
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
	newDevToken, ok, _ := s.dbSessions.renewFromDevice(r.Context(), dc.Value, clientIP(r, s.cfg.Server.TrustProxy), r.Header.Get("User-Agent"), s.cfg.Server.DeviceBinding)
	if !ok {
		// On strict-drift the row is already hard-revoked inside renewFromDevice;
		// clearing the cookie + destroying the session here covers both the
		// drift-reject and the benign expiry case.
		_ = s.sm.Destroy(r.Context())
		clearDeviceCookie(w)
		jsonErr(w, "device token invalid or expired", http.StatusUnauthorized)
		return
	}
	s.sm.Put(r.Context(), "authenticated", true)
	if newDevToken != "" {
		setDeviceCookie(w, newDevToken, s.secureCookies())
	}
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
	_ = s.sm.Destroy(r.Context())
	if s.dbSessions != nil {
		if dc, err := r.Cookie(deviceCookie); err == nil {
			_ = s.dbSessions.revokeDevice(r.Context(), dc.Value)
		}
	}
	clearDeviceCookie(w)
	jsonOK(w, map[string]string{"status": "ok"})
}

// apiLogoutAll revokes all sessions and trusted devices (emergency lockout).
func (s *Server) apiLogoutAll(w http.ResponseWriter, r *http.Request) {
	if s.scsStore != nil {
		_ = s.scsStore.deleteAll()
	}
	if s.dbSessions != nil {
		_ = s.dbSessions.revokeAllDevices(r.Context())
	}
	_ = s.sm.Destroy(r.Context())
	clearDeviceCookie(w)
	jsonOK(w, map[string]string{"status": "ok"})
}
