package webui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/dicode/dicode/pkg/ipc"
	"go.uber.org/zap"
)

// webhookAuthGuard checks whether the task associated with the requested webhook
// path has trigger.auth: true. If it does, and the request carries no valid
// dicode session, the request is rejected (401 JSON for API callers, redirect
// for browsers). Public webhooks (no auth: true) pass through unchanged.
//
// The match is longest-prefix so overlapping paths resolve to the most specific
// spec — the gateway already routes this way, and the auth decision must agree.
// With a short-prefix-wins loop, a public `/hooks/ai` would shadow a private
// `/hooks/ai/dicodai` and silently drop `auth: true` at the door.
func (s *Server) webhookAuthGuard(w http.ResponseWriter, r *http.Request, next http.Handler) {
	var requiresAuth bool
	var bestLen = -1
	for _, spec := range s.registry.All() {
		wp := spec.Trigger.Webhook
		if wp == "" || !ipc.PathMatches(wp, r.URL.Path) {
			continue
		}
		// Compare normalised pattern length so a trailing slash on the
		// registered webhook cannot artificially shorten the match and
		// shadow a longer protected path.
		l := len(strings.TrimSuffix(wp, "/"))
		if l > bestLen {
			bestLen = l
			requiresAuth = spec.Trigger.WebhookAuth
		}
	}

	if !requiresAuth {
		next.ServeHTTP(w, r)
		return
	}

	// Task requires a valid dicode session.
	if s.hasValidSession(r) {
		next.ServeHTTP(w, r)
		return
	}

	// For webhook paths, a GET from a browser includes "text/html" in Accept.
	// API clients (curl, fetch without custom headers, etc.) typically don't,
	// so we use that to decide redirect vs 401.
	isBrowserGet := r.Method == http.MethodGet &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
	if !isBrowserGet {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	escaped := url.QueryEscape(r.URL.RequestURI())
	http.Redirect(w, r, "/login?next="+escaped, http.StatusSeeOther)
}

// isSafeNextPath returns true when next is a same-origin, path-only URL
// suitable for use as a redirect target. Rejects empty strings, non-leading-
// slash values, protocol-relative URLs (//foo), URLs containing a backslash
// (Windows-style separator confusion), and anything with a scheme or host.
// This is a same-origin check, not a path-normalisation check: validated
// values may still contain %-encoded bytes that are URI-legal but surprising
// (e.g. %00). Callers writing validated values into filesystem paths or
// logs must apply their own escaping.
func isSafeNextPath(next string) bool {
	if next == "" || next[0] != '/' {
		return false
	}
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return false
	}
	if strings.ContainsAny(next, "\\\r\n") {
		return false
	}
	u, err := url.Parse(next)
	if err != nil {
		return false
	}
	if u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return false
	}
	return true
}

// hasValidSession returns true if r carries a valid in-memory session cookie
// or a device token that can be auto-renewed. Note: when a device token is
// present the function has a side effect — it consumes the token and issues a
// fresh one via renewFromDevice.
func (s *Server) hasValidSession(r *http.Request) bool {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if s.sessions.valid(cookie.Value) {
			return true
		}
	}
	if s.dbSessions != nil {
		if dc, err := r.Cookie(deviceCookie); err == nil {
			_, ok := s.dbSessions.renewFromDevice(r.Context(), dc.Value, clientIP(r, s.cfg.Server.TrustProxy))
			return ok
		}
	}
	return false
}

// requireSessionOrAPIKey accepts either a session cookie OR a Bearer API key.
// Used by endpoints that need to be reachable from both the WebUI (cookies)
// and CLI/CI tooling (Bearer). When server.auth is disabled this is a no-op.
func (s *Server) requireSessionOrAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Server.Auth {
			next.ServeHTTP(w, r)
			return
		}
		if raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); raw != "" && s.apiKeys.validate(r.Context(), raw) {
			next.ServeHTTP(w, r)
			return
		}
		if s.hasValidSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="dicode"`)
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
	})
}

// requireAuth is a middleware that enforces authentication when server.auth is
// enabled. API requests receive a 401 JSON response; browser requests are
// redirected to the login page. Public paths (login endpoint, static assets,
// service worker) are always allowed through.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Server.Auth {
			next.ServeHTTP(w, r)
			return
		}

		// Always-public paths: login endpoint and static assets needed to
		// render the login page. Webhooks are handled separately via HMAC.
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Check session cookie first.
		if cookie, err := r.Cookie(sessionCookie); err == nil {
			if s.sessions.valid(cookie.Value) {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Session missing or expired — try device token for auto-renewal.
		if dc, err := r.Cookie(deviceCookie); err == nil {
			if newDevToken, ok := s.dbSessions.renewFromDevice(r.Context(), dc.Value, clientIP(r, s.cfg.Server.TrustProxy)); ok {
				setSessionCookie(w, s.sessions.issue())
				if newDevToken != "" {
					setDeviceCookie(w, newDevToken)
				}
				next.ServeHTTP(w, r)
				return
			}
		}

		if isAPIRequest(r) {
			jsonErr(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// corsMiddleware replaces the inline wildcard CORS with a configurable
// allowlist. When AllowedOrigins is empty only same-origin requests are served
// (no Access-Control-Allow-Origin header emitted).
// Origins are validated with url.Parse at startup; malformed entries are
// silently skipped so a config typo can't accidentally open the allowlist.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	origins := make(map[string]bool, len(s.cfg.Server.AllowedOrigins))
	for _, o := range s.cfg.Server.AllowedOrigins {
		o = strings.TrimRight(o, "/")
		parsed, err := url.Parse(o)
		if err != nil || parsed.Host == "" || parsed.Scheme == "" {
			s.log.Warn("invalid allowed_origins entry — skipping", zap.String("origin", o))
			continue
		}
		origins[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && origins[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders adds baseline security headers to every response.
// This replaces the package-level function so we can include a CSP.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// CSP: tighten script/connect surface while keeping Monaco CDN and
		// the esm.sh CDN (used by Lit) functional.
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' https://cdn.jsdelivr.net https://esm.sh; "+
				"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
				"connect-src 'self' ws: wss: https://esm.sh https://cdn.jsdelivr.net; "+
				"worker-src 'self' blob:; "+
				"font-src 'self' https://cdn.jsdelivr.net; "+
				"img-src 'self' data:;",
		)
		next.ServeHTTP(w, r)
	})
}

// isPublicPath returns true for paths that must remain accessible without a
// session: the login/unlock endpoints, the webhook UI, and the dicode SDK.
func isPublicPath(path string) bool {
	switch {
	case path == "/api/auth/login",
		path == "/api/auth/refresh",
		path == "/login",
		path == "/dicode.js",
		path == "/dicode-oauth-broadcast.js",
		strings.HasPrefix(path, webhookPathPrefix):
		return true
	}
	return false
}

// isAPIRequest returns true when the request looks like a programmatic API
// call rather than a browser navigation. Used to decide 401 vs redirect.
func isAPIRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") ||
		strings.HasPrefix(r.URL.Path, "/mcp") ||
		strings.HasPrefix(r.URL.Path, webhookPathPrefix) ||
		r.URL.Path == "/ws" {
		return true
	}
	return r.Header.Get("Accept") == "application/json" ||
		strings.Contains(r.Header.Get("Content-Type"), "application/json")
}

// clientIP extracts the real client IP. X-Forwarded-For is only trusted when
// trustProxy is true (i.e. server.trust_proxy: true in config), so that direct
// clients cannot spoof their IP and bypass rate limiting.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if idx := strings.Index(fwd, ","); idx != -1 {
				return strings.TrimSpace(fwd[:idx])
			}
			return strings.TrimSpace(fwd)
		}
	}
	ip, _, _ := splitHostPort(r.RemoteAddr)
	return ip
}

// splitHostPort is a nil-safe wrapper around net.SplitHostPort.
func splitHostPort(hostport string) (host, port string, err error) {
	// Fast path: avoid importing net just for this.
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		return hostport[:i], hostport[i+1:], nil
	}
	return hostport, "", nil
}
