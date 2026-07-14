package webui

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// auditDenied records a denied-auth event (#45). Best-effort and nil-safe;
// uses a background context so a client disconnect cannot cancel the insert.
func (s *Server) auditDenied(r *http.Request, reason string) {
	s.audit.Emit(context.Background(), audit.Event{
		EventType:  audit.EventDenied,
		ActorKind:  "ip",
		ActorID:    clientIP(r, s.cfg.Server.TrustProxy),
		TargetKind: "endpoint",
		TargetID:   r.Method + " " + r.URL.Path,
		Allowed:    false,
		Reason:     reason,
	})
}

// relayAuthBlockedPage explains, to a browser that reached a session-gated
// webhook through the public relay URL, why it can't be served there and where
// to go instead. Printf args: %d = daemon port, %s = webhook path.
const relayAuthBlockedPage = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Login required — not available via relay</title>
<style>
  body { font-family: system-ui, sans-serif; background: #1e1e2e; color: #cdd6f4;
         margin: 0; display: flex; min-height: 100vh; align-items: center; justify-content: center; }
  main { max-width: 34rem; padding: 2rem; }
  h1 { color: #f9e2af; font-size: 1.3rem; margin: 0 0 1rem; }
  p { line-height: 1.6; margin: 0 0 1rem; }
  code { background: #302d41; padding: .15em .4em; border-radius: 4px; font-size: .9em; word-break: break-all; }
  a { color: #89b4fa; }
</style>
</head>
<body>
<main>
<h1>This page needs a login — and the public relay URL can't provide one</h1>
<p>Session logins never travel over the relay (it forwards webhooks only and
strips credentials), so a page behind <code>auth: true</code> can't be served here.</p>
<p>Open it on your dicode server's own address, e.g.
<code>http://YOUR-DICODE-HOST:%d%s</code>, or reach your server remotely with a
tunnel such as <a href="https://tailscale.com/">Tailscale</a> or
<a href="https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/">cloudflared</a>.</p>
</main>
</body>
</html>`

// webhookAuthGuard checks whether the task associated with the requested webhook
// path has trigger.auth: true. If it does, the request is rejected unless it
// carries a valid dicode session. A request arriving via the relay (trusted
// X-Relay-Base) can never be authenticated and is refused outright — HTML
// explainer for browsers, JSON 401 otherwise. A direct request with no session
// gets a JSON 401 for API callers or a /login redirect for browsers. Public
// webhooks (no auth: true) pass through unchanged.
//
// The match is longest-prefix so overlapping paths resolve to the most specific
// spec — the gateway already routes this way, and the auth decision must agree.
// With a short-prefix-wins loop, a public `/hooks/ai` would shadow a private
// `/hooks/ai/dicodai` and silently drop `auth: true` at the door.
func (s *Server) webhookAuthGuard(w http.ResponseWriter, r *http.Request, next http.Handler) {
	var requiresAuth bool
	var bestLen = -1
	// Consider every task kind: a kind: PipelineTask can also carry a webhook
	// trigger with auth: true, and its protection must be enforced here just
	// like a kind: Task's. Using AllKinded (not All) is what keeps a pipeline
	// webhook from being silently public.
	for _, k := range s.registry.AllKinded() {
		var wp string
		var auth bool
		switch t := k.(type) {
		case *task.Spec:
			wp, auth = t.Trigger.Webhook, t.Trigger.WebhookAuth
		case *task.PipelineTask:
			wp, auth = t.Trigger.Webhook, t.Trigger.WebhookAuth
		default:
			continue
		}
		if wp == "" || !ipc.PathMatches(wp, r.URL.Path) {
			continue
		}
		// Compare normalised pattern length so a trailing slash on the
		// registered webhook cannot artificially shorten the match and
		// shadow a longer protected path.
		l := len(strings.TrimSuffix(wp, "/"))
		if l > bestLen {
			bestLen = l
			requiresAuth = auth
		}
	}

	if !requiresAuth {
		next.ServeHTTP(w, r)
		return
	}

	// A relayed request carries a trusted X-Relay-Base header — the forwarder
	// stamps it and drops any inbound copy. Such a request can never carry a
	// legitimate session: the relay strips every credential header (cookie,
	// authorization, x-api-key) and forwards only /hooks/*, so /login is
	// unreachable and no cookie survives the hop. Reject an auth-gated webhook
	// here BEFORE evaluating any session, so a credential that somehow survived
	// forwarding can never authenticate a relayed request — the forwarder
	// strip-list stays defense-in-depth, not the load-bearing control.
	if r.Header.Get("X-Relay-Base") != "" {
		s.auditDenied(r, "auth-gated webhook not reachable via relay")
		// A human who clicked the public relay link gets an explainer page
		// pointing at the daemon's own address / a tunnel; API callers get JSON.
		// Without this a browser renders the raw JSON error in the viewport.
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, relayAuthBlockedPage, s.port, html.EscapeString(r.URL.Path))
			return
		}
		jsonErr(w, "this webhook requires authentication and is not reachable via the public relay URL; open it on the daemon's own address, or reach your server via a tunnel (Tailscale/cloudflared)", http.StatusUnauthorized)
		return
	}

	// Task requires a valid dicode session.
	if s.hasValidSession(w, r) {
		next.ServeHTTP(w, r)
		return
	}

	s.auditDenied(r, "webhook requires authenticated session")

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

// hasValidSession returns true if the request carries a valid scs session
// or a device token that can be auto-renewed. On a strict-drift reject the
// offending device row is hard-revoked inside renewFromDevice; w lets this path
// clear the now-dead device cookie so the browser stops replaying it.
func (s *Server) hasValidSession(w http.ResponseWriter, r *http.Request) bool {
	if s.sm.GetBool(r.Context(), "authenticated") {
		return true
	}
	if s.dbSessions != nil {
		if dc, err := r.Cookie(deviceCookie); err == nil {
			_, ok, driftReject := s.dbSessions.renewFromDevice(r.Context(), dc.Value, clientIP(r, s.cfg.Server.TrustProxy), r.Header.Get("User-Agent"), s.cfg.Server.DeviceBinding)
			if !ok && driftReject {
				clearDeviceCookie(w)
			}
			return ok
		}
	}
	return false
}

// requireSessionOrAPIKey accepts either a session cookie OR a Bearer API key.
// Used by endpoints that need to be reachable from both the WebUI (cookies)
// and CLI/CI tooling (Bearer). When server.auth is disabled this is a no-op.
func (s *Server) requireSessionOrAPIKey(next http.Handler) http.Handler {
	return s.sessionOrAPIKeyMiddleware(s.apiKeys.validate)(next)
}

// requireSessionOrNonEphemeralAPIKey is requireSessionOrAPIKey that rejects
// ephemeral per-run MCP tokens on the Bearer path (a session cookie is still
// accepted). Governance endpoints an agent must not self-serve — task
// approval above all — gate on this (see validateNonEphemeral).
func (s *Server) requireSessionOrNonEphemeralAPIKey(next http.Handler) http.Handler {
	return s.sessionOrAPIKeyMiddleware(s.apiKeys.validateNonEphemeral)(next)
}

func (s *Server) sessionOrAPIKeyMiddleware(validate func(context.Context, string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.cfg.Server.Auth {
				next.ServeHTTP(w, r)
				return
			}
			if raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); raw != "" && validate(r.Context(), raw) {
				next.ServeHTTP(w, r)
				return
			}
			if s.hasValidSession(w, r) {
				next.ServeHTTP(w, r)
				return
			}
			s.auditDenied(r, "no valid session or API key")
			w.Header().Set("WWW-Authenticate", `Bearer realm="dicode"`)
			jsonErr(w, "unauthorized", http.StatusUnauthorized)
		})
	}
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

		// Check scs session first.
		if s.sm.GetBool(r.Context(), "authenticated") {
			next.ServeHTTP(w, r)
			return
		}

		// Session missing or expired — try device token for auto-renewal.
		if dc, err := r.Cookie(deviceCookie); err == nil {
			newDevToken, ok, driftReject := s.dbSessions.renewFromDevice(r.Context(), dc.Value, clientIP(r, s.cfg.Server.TrustProxy), r.Header.Get("User-Agent"), s.cfg.Server.DeviceBinding)
			if ok {
				s.sm.Put(r.Context(), "authenticated", true)
				if newDevToken != "" {
					s.cfgMu.RLock()
					secure := secureCookies(s.cfg)
					s.cfgMu.RUnlock()
					setDeviceCookie(w, newDevToken, secure)
				}
				next.ServeHTTP(w, r)
				return
			}
			// A strict-drift reject already hard-revoked the row; clear the now-
			// dead cookie so the browser stops replaying it.
			if driftReject {
				clearDeviceCookie(w)
			}
		}

		s.auditDenied(r, "no valid session")
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
// clients cannot spoof their IP and bypass rate limiting. The returned value is
// a bare address (no port, no IPv6 brackets) so it parses with netip.ParseAddr
// for subnet binding.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first := fwd
			if idx := strings.Index(fwd, ","); idx != -1 {
				first = fwd[:idx]
			}
			// A leftmost token that normalizes to "" (e.g. a client-supplied
			// "[]") must not erase the client IP — fall back to RemoteAddr so
			// callers (rate-limiter, device binding) always get a bound value.
			if ip := normalizeIP(strings.TrimSpace(first)); ip != "" {
				return ip
			}
		}
	}
	return normalizeIP(r.RemoteAddr)
}

// normalizeIP strips any port and IPv6 brackets so the result is a bare address
// that netip.ParseAddr accepts. Inputs may be "host:port", "[v6]:port", "[v6]",
// or a bare address; anything unparseable is returned trimmed of brackets.
func normalizeIP(s string) string {
	if s == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host // SplitHostPort already strips IPv6 brackets
	}
	// No port present: drop surrounding brackets on a bare IPv6 literal.
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return s
}
