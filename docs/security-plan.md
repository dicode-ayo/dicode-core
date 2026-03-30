# Security Plan — dicode

> **Goal**: Protect the WebUI, REST API, MCP endpoint, and webhook triggers with
> layered authentication, a "trusted browser" mechanism, and a foundation that
> scales to multi-user RBAC without a full rewrite.
>
> This plan is additive: each phase is independently shippable and does not
> break the phase before it.

---

## Threat Model

| Threat | Current risk | Target state |
|---|---|---|
| Unauthenticated task execution via `/api/tasks/{id}/run` | HIGH — fully open | Blocked by global auth |
| Unauthenticated webhook trigger via `/hooks/*` | HIGH — open, path is only "secret" | Blocked by HMAC signature |
| Config exfiltration/modification via `/api/config/raw` | HIGH — open | Blocked by global auth |
| MCP lateral movement (run_task, switch_dev_mode) | HIGH — MCP is open JSON-RPC | Blocked by API key |
| Secrets theft via `/api/secrets` | MEDIUM — passphrase-gated | Preserved + extended |
| CSRF | LOW — SameSite=Strict cookie on secrets | Extended to all session cookies |
| Session hijacking | MEDIUM — in-memory tokens (lost on restart) | SQLite-backed, rotatable |
| Webhook replay attacks | MEDIUM — no timestamp validation | Mitigated by HMAC + timestamp |
| Timing attacks on password compare | LOW — already uses `subtle.ConstantTimeCompare` | Preserved |
| Clickjacking | LOW — X-Frame-Options set | Preserved |
| CORS overreach | MEDIUM — `Access-Control-Allow-Origin: *` | Tightened to configurable allowlist |

### Trust boundary assumption

dicode is self-hosted. The intended deployment is:

```
internet ──TLS──▶ reverse proxy (nginx/Caddy) ──▶ dicode :8080 (localhost)
```

dicode handles auth itself; TLS should be terminated at the proxy. The plan
also covers scenarios where dicode is exposed directly (e.g. on a LAN without a
proxy).

---

## Phase 1 — Global Authentication (Whole-Server Auth Wall)

**Status**: Not started
**Scope**: `pkg/webui/server.go`, `pkg/config/config.go`
**Breaks**: Nothing — opt-in via `server.auth: true` in config

### What & Why

Right now `server.secret` only gates `/api/secrets` and `/api/config/raw`.
Everything else — task runs, logs, WebSocket, MCP, AI stream, runtime install —
is completely open. Phase 1 extends the existing passphrase session to cover the
whole server.

### Config addition

```yaml
server:
  auth: true            # default: false (backwards-compatible)
  secret: "..."         # existing field — now also used as server passphrase
  allowed_origins: []   # CORS: empty = same-origin only, "*" = any (explicit opt-in)
```

### Routes that stay public (login flow cannot be gated)

| Route | Reason |
|---|---|
| `POST /api/secrets/unlock` | Login endpoint itself |
| `GET /app/*` | Static assets for the login page |
| `GET /sw.js` | Service worker |

All other routes go through a new `requireAuth` middleware.

### Implementation sketch

```go
// requireAuth checks the session cookie; redirects browser, 401s API calls.
func (s *Server) requireAuth(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !s.cfg.Server.Auth {
            next.ServeHTTP(w, r)
            return
        }
        cookie, err := r.Cookie(secretsCookie)
        if err != nil || !s.sessions.valid(cookie.Value) {
            if isAPIRequest(r) {
                http.Error(w, "unauthorized", http.StatusUnauthorized)
            } else {
                http.Redirect(w, r, "/?auth=required", http.StatusSeeOther)
            }
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

### CORS hardening

Replace the blanket `Access-Control-Allow-Origin: *` with:

```go
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
    allowed := buildAllowedOrigins(s.cfg.Server.AllowedOrigins)
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        origin := r.Header.Get("Origin")
        if allowed.contains(origin) {
            w.Header().Set("Access-Control-Allow-Origin", origin)
            w.Header().Set("Vary", "Origin")
        }
        // ... methods/headers as before
    })
}
```

When `allowed_origins` is empty, no CORS header is emitted (same-origin only).

### Additional security headers (add to `securityHeaders`)

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:;
Permissions-Policy: camera=(), microphone=(), geolocation=()
```

---

## Phase 2 — Trusted Browser / Persistent Sessions

**Status**: Not started
**Depends on**: Phase 1
**Scope**: `pkg/webui/server.go`, `pkg/db/sqlite.go`, new `pkg/webui/sessions.go`

### What & Why

Currently sessions live only in RAM — a restart logs everyone out. The
"trusted browser" concept works like this:

1. User logs in with passphrase → server issues two tokens:
   - **Session token** (short-lived, 8 h): controls current browser tab access
   - **Device token** (long-lived, 30 d): marks this browser as trusted
2. On subsequent visits: session expired → server checks device token → if valid,
   auto-issues new session token (transparent re-auth)
3. User sees a "Trusted devices" panel and can revoke individual devices

This is conceptually similar to "remember this device" on banking apps, but
kept simple: the passphrase is the single credential; the device token is the
"memory" of that credential being previously validated.

### Database schema

```sql
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,          -- opaque token (stored as SHA-256 hash)
    token_hash  TEXT NOT NULL UNIQUE,      -- SHA-256(raw_token) — never store raw
    kind        TEXT NOT NULL,             -- 'session' | 'device'
    user_agent  TEXT,
    ip          TEXT,
    created_at  DATETIME NOT NULL,
    last_seen   DATETIME NOT NULL,
    expires_at  DATETIME NOT NULL
);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);
```

### Token lifecycle

```
POST /api/secrets/unlock  { "password": "..." }
  │
  ├─ rate limit check (existing, 5/min/IP)
  ├─ constant-time passphrase compare (existing)
  │
  ├─ issue session token  → Set-Cookie: dicode-session=<token>; HttpOnly; SameSite=Strict; MaxAge=8h
  └─ issue device token   → Set-Cookie: dicode-device=<token>;  HttpOnly; SameSite=Strict; MaxAge=30d; Path=/api/secrets/unlock
```

The device token cookie has `Path=/api/secrets/unlock` so it is only sent
to the unlock endpoint — it never leaks into other requests.

### Auto-renew flow

```
requireAuth middleware
  │
  ├─ valid session cookie → pass through
  │
  └─ no/expired session cookie
       │
       └─ check dicode-device cookie (sent to /api/secrets/unlock path)
            │  (client JS calls POST /api/secrets/unlock with device token in body)
            ├─ valid device token → issue new session token → pass through
            └─ invalid / expired → 401 / redirect to login
```

Client-side: the SPA should intercept 401 responses, attempt device-token
refresh (`POST /api/auth/refresh`), then retry the original request once.

### Device management API

```
GET  /api/auth/devices        → list trusted devices (id, user_agent, ip, last_seen, expires_at)
DELETE /api/auth/devices/{id} → revoke a trusted device
POST /api/auth/logout         → revoke current session + optionally current device
POST /api/auth/logout-all     → revoke all sessions and devices (emergency)
```

### Security notes

- Store only `SHA-256(raw_token)` in the DB; the raw token lives only in the
  cookie and the HTTP response body (never logged).
- Rotate device tokens on use (issue a new one, invalidate the old) to detect
  token theft via cookie replay.
- Purge expired rows with a nightly SQLite `DELETE WHERE expires_at < now()`.
- Add `Secure` flag to all auth cookies in production (configurable flag or
  auto-detect via `X-Forwarded-Proto: https`).

---

## Phase 3 — Webhook HMAC Authentication

**Status**: Not started
**Depends on**: Phase 1 (or standalone — webhooks are a separate concern)
**Scope**: `pkg/trigger/engine.go`, `pkg/task/task.go` (task spec), `pkg/webui/server.go`

### What & Why

Webhooks are currently path-only security — knowing the path is the only
"credential". This is inadequate for any task that mutates state or costs money
to run. We adopt GitHub's battle-tested HMAC-SHA256 signature scheme.

### Task spec addition

```yaml
trigger:
  webhook:
    path: /my-webhook
    secret: "${WEBHOOK_SECRET}"   # resolved from secrets chain
    hmac_header: X-Hub-Signature-256  # default; or X-Dicode-Signature
    max_age_seconds: 300          # replay protection window (default: 300)
```

When `secret` is absent, the webhook is unauthenticated (backwards-compatible).

### Signature format (GitHub-compatible)

Sender computes:
```
signature = "sha256=" + hex(HMAC-SHA256(secret, raw_request_body))
```

And sends it in the `X-Hub-Signature-256` header. This means existing GitHub
webhook integrations can point at dicode without any client changes — just set
the same secret in both GitHub and the task spec.

### Replay protection

Include a timestamp header:
```
X-Dicode-Timestamp: 1711234567   # Unix epoch
```

Server rejects requests where `|now - timestamp| > max_age_seconds`.

When consuming GitHub webhooks (which don't send a timestamp), disable replay
protection or use a shorter HMAC window.

### Implementation in `WebhookHandler`

```go
func (e *Engine) verifyWebhookSignature(spec *task.Spec, r *http.Request, body []byte) error {
    secret := spec.Trigger.Webhook.Secret
    if secret == "" {
        return nil // unauthenticated webhook — allowed for backwards-compat
    }

    // Replay protection
    if spec.Trigger.Webhook.MaxAge > 0 {
        ts := r.Header.Get("X-Dicode-Timestamp")
        // validate timestamp within window...
    }

    got := r.Header.Get(spec.Trigger.Webhook.HMACHeader)
    want := "sha256=" + computeHMAC(secret, body)
    if !hmac.Equal([]byte(got), []byte(want)) { // constant-time
        return ErrInvalidSignature
    }
    return nil
}
```

### Alternate: Bearer token (simpler senders)

For callers that cannot compute HMAC, support:
```
Authorization: Bearer <static-token>
```
Where the token is configured in the task spec as `auth_token`. This is
weaker (no body integrity) but acceptable for internal automation.

Priority: HMAC wins if both are set.

---

## Phase 4 — MCP API Key Authentication

**Status**: Not started
**Depends on**: Phase 1
**Scope**: `pkg/mcp/server.go`, `pkg/webui/server.go`, `pkg/db/sqlite.go`

### What & Why

The MCP endpoint (`/mcp`) exposes `run_task`, `switch_dev_mode`, and full task
enumeration. It is currently completely open JSON-RPC 2.0. Any process that
can reach the server can run arbitrary tasks.

MCP clients (Claude, other agents) send HTTP headers — API key auth is the
natural fit.

### API key schema

```sql
CREATE TABLE IF NOT EXISTS api_keys (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,           -- human label ("Claude Desktop", "CI bot")
    key_hash    TEXT NOT NULL UNIQUE,    -- SHA-256(raw_key)
    prefix      TEXT NOT NULL,           -- first 8 chars of raw key (for display)
    created_at  DATETIME NOT NULL,
    last_used   DATETIME,
    expires_at  DATETIME                 -- NULL = no expiry
);
```

### Key format

```
dck_<base64url(32 random bytes)>
```

Example: `dck_A3kR9pQzWmXvY2nJ5sLt8dCf0hBuNrEa`

Prefix `dck_` makes it greppable/identifiable in logs and `.env` files.

### Authentication

```
POST /mcp
Authorization: Bearer dck_A3kR9pQzWmXvY2nJ5sLt8dCf0hBuNrEa
```

Middleware on the MCP mount point:
```go
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !s.cfg.Server.Auth {
            next.ServeHTTP(w, r)
            return
        }
        key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
        if !s.db.ValidAPIKey(r.Context(), key) {
            writeJSONError(w, http.StatusUnauthorized, "invalid or missing API key")
            return
        }
        s.db.TouchAPIKey(r.Context(), key) // update last_used
        next.ServeHTTP(w, r)
    })
}
```

### Key management API

```
POST   /api/auth/keys           → generate a new key (returns raw key once)
GET    /api/auth/keys           → list keys (id, name, prefix, last_used, expires_at)
DELETE /api/auth/keys/{id}      → revoke a key
```

The raw key is returned **only once** at creation — thereafter only the prefix
and hash are accessible (same pattern as GitHub PATs).

---

## Phase 5 — Multi-User RBAC (North Star)

**Status**: Not started — design only
**Depends on**: Phases 1–4

### What & Why

Today dicode is single-user. For teams, we need:
- Per-user credentials (no shared passphrase)
- Role-based access to limit what each user can do
- Per-user trusted devices and API keys

### Roles

| Role | Capabilities |
|---|---|
| `admin` | Everything: manage users, config, secrets, run tasks |
| `operator` | Run tasks, view runs/logs, manage own devices/keys |
| `viewer` | Read-only: list tasks, view runs and logs |

### User schema

```sql
CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,   -- argon2id (replaces single passphrase)
    role            TEXT NOT NULL DEFAULT 'operator',
    created_at      DATETIME NOT NULL,
    last_login      DATETIME
);
```

**Password hashing**: Use `argon2id` (Go: `golang.org/x/crypto/argon2`) with
parameters `m=64MB, t=3, p=4`. This is the current OWASP recommendation,
stronger than bcrypt for modern hardware.

### Migration path from single-user

When `server.auth: true` and no users exist in the DB, bootstrap by creating
a single `admin` user with `username: admin` and password from `server.secret`.
On next startup with users in the DB, `server.secret` is ignored for auth.
This is a seamless upgrade.

### API key scoping

In Phase 4, API keys are instance-scoped. In Phase 5, they become user-scoped:
```sql
ALTER TABLE api_keys ADD COLUMN user_id TEXT REFERENCES users(id);
```

API key permissions are capped at the issuing user's role.

### Session user binding

```sql
ALTER TABLE sessions ADD COLUMN user_id TEXT REFERENCES users(id);
```

The `requireAuth` middleware populates a `userID` context value; downstream
handlers use it for authorization checks:

```go
func requireRole(role string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            u := userFromContext(r.Context())
            if !u.HasRole(role) {
                http.Error(w, "forbidden", http.StatusForbidden)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

### Audit log

All state-changing actions (task run, config save, secret set, user create)
should be logged to an `audit_log` table:

```sql
CREATE TABLE IF NOT EXISTS audit_log (
    id          TEXT PRIMARY KEY,
    user_id     TEXT,
    action      TEXT NOT NULL,   -- "task.run", "secret.set", "config.save", ...
    target      TEXT,            -- task ID, secret key, etc.
    ip          TEXT,
    created_at  DATETIME NOT NULL
);
```

---

## WebUI Standalone Consideration

> Referenced from dicode-core PR #9: the WebUI may become a standalone
> deployable task.

If the WebUI moves to a separate origin:

1. **Cookie auth breaks across origins.** Switch to **short-lived JWTs** for
   the SPA session (the `Authorization: Bearer <jwt>` header pattern). JWTs
   should be stored in `sessionStorage` (not `localStorage`) and have a 1-hour
   expiry. Refresh via the device token flow.

2. **CORS must be explicitly configured.** The WebUI origin (e.g.
   `https://ui.dicode.internal`) must be in `server.allowed_origins`.

3. **The device trust cookie** can still work if both the API and UI share a
   parent domain (e.g. `*.dicode.internal`) using `Domain=.dicode.internal`.
   Cross-domain? Store the device token server-side and issue a re-auth URL
   (magic link) instead.

4. **Static asset serving** from the Go binary becomes optional. The embedded
   `static/` FS and SPA catch-all can be toggled off with `server.serve_ui: false`.

Plan the auth API (Phase 1 + 2) with this in mind: the `/api/auth/*` endpoints
should be fully usable with `Authorization` header auth (no cookie required)
so the standalone WebUI can work without cookie sharing.

---

## Implementation Order & Priority

| Phase | Priority | Effort | Risk |
|---|---|---|---|
| 1 — Global auth wall | **P0** | Small (extend existing middleware) | Low |
| 3 — Webhook HMAC | **P0** | Medium | Low |
| 4 — MCP API key | **P1** | Medium | Low |
| 2 — Trusted browser | **P1** | Medium | Medium |
| 5 — Multi-user RBAC | **P2 (north star)** | Large | High |

Phases 1 and 3 are the most urgent: they close the completely open task
execution and webhook attack surface.

---

## Vulnerabilities to Fix in Parallel (Low-Effort)

These are not phase-gated — they should be fixed alongside Phase 1:

1. **CORS `*` → allowlist** — Change `Access-Control-Allow-Origin: *` in
   [server.go:295](../pkg/webui/server.go#L295) to use the new `allowed_origins`
   config field. Default: empty (same-origin only).

2. **Add CSP header** — Extend `securityHeaders` with a Content-Security-Policy
   that blocks inline scripts and restricts `connect-src` to `'self'` and
   WebSocket. This kills XSS escalation paths even if one is found.

3. **Secure + SameSite flags on all cookies** — The device cookie in Phase 2
   must also set `Secure` when TLS is detected (check `X-Forwarded-Proto` or
   add a `server.tls: true` config flag).

4. **WriteTimeout on non-streaming routes** — The server intentionally has no
   `WriteTimeout` (for WS/SSE). Add a per-route timeout on regular REST
   endpoints using `http.TimeoutHandler` to limit Slowloris-style attacks.

5. **Task ID validation at API layer** — Currently the server trusts the
   registry to return "not found" for unknown IDs. Add explicit `[a-zA-Z0-9_/-]+`
   validation at the route level to shrink the attack surface.

---

## Deployment Checklist (Ops)

Before exposing dicode outside localhost:

- [ ] `server.auth: true` in `dicode.yaml`
- [ ] Strong passphrase in `server.secret` (≥16 chars, high entropy)
- [ ] TLS terminated at the reverse proxy (nginx/Caddy)
- [ ] `server.allowed_origins` set to the exact WebUI origin
- [ ] Webhook tasks use `secret:` field (Phase 3)
- [ ] MCP key generated and set in Claude/agent config (Phase 4)
- [ ] Firewall: dicode port not directly reachable from internet
- [ ] `dicode.yaml` not world-readable (`chmod 600`)

---

*Last updated: 2026-03-29*
