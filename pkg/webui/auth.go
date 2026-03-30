package webui

import (
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"
)

// webhookAuthGuard checks whether the task associated with the requested webhook
// path has trigger.auth: true. If it does, and the request carries no valid
// dicode session, the request is rejected (401 JSON for API callers, redirect
// for browsers). Public webhooks (no auth: true) pass through unchanged.
func (s *Server) webhookAuthGuard(w http.ResponseWriter, r *http.Request, next http.Handler) {
	// Find the spec whose webhook path is a prefix-match for r.URL.Path.
	var requiresAuth bool
	for _, spec := range s.registry.All() {
		wp := spec.Trigger.Webhook
		if wp == "" {
			continue
		}
		if r.URL.Path == wp || strings.HasPrefix(r.URL.Path, wp+"/") {
			requiresAuth = spec.Trigger.WebhookAuth
			break
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

	// For webhook paths, a GET without an API content/accept header is a
	// browser navigating to the task UI — redirect rather than 401.
	isBrowserGet := r.Method == http.MethodGet &&
		r.Header.Get("Accept") != "application/json" &&
		!strings.Contains(r.Header.Get("Content-Type"), "application/json")
	if !isBrowserGet {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/?auth=required", http.StatusSeeOther)
}

// hasValidSession returns true if r carries a valid in-memory session cookie
// or a device token that can be auto-renewed.
func (s *Server) hasValidSession(r *http.Request) bool {
	if cookie, err := r.Cookie(secretsCookie); err == nil {
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
		if cookie, err := r.Cookie(secretsCookie); err == nil {
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
		http.Redirect(w, r, "/?auth=required", http.StatusSeeOther)
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
				"style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self' ws: wss: https://esm.sh; "+
				"font-src 'self' https://cdn.jsdelivr.net; "+
				"img-src 'self' data:;",
		)
		next.ServeHTTP(w, r)
	})
}

// isPublicPath returns true for paths that must remain accessible without a
// session: static assets, the service worker, and the login/unlock endpoints.
func isPublicPath(path string) bool {
	switch {
	case path == "/api/secrets/unlock",
		path == "/api/auth/refresh",
		strings.HasPrefix(path, "/app/"),
		path == "/sw.js":
		return true
	}
	return false
}

// isAPIRequest returns true when the request looks like a programmatic API
// call rather than a browser navigation. Used to decide 401 vs redirect.
func isAPIRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") ||
		strings.HasPrefix(r.URL.Path, "/mcp") ||
		strings.HasPrefix(r.URL.Path, "/hooks/") ||
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
