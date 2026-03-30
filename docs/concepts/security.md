# Security — Developer Reference

This document covers the full security architecture implemented across `pkg/webui/auth.go`, `pkg/webui/sessions_db.go`, `pkg/webui/apikeys.go`, `pkg/webui/server.go`, and `pkg/trigger/engine.go`. It is intended for contributors modifying the auth system and for operators who need to understand the trust model.

---

## Overview

Security is **opt-in**. Without `server.auth: true` in `dicode.yaml`, all behaviour is identical to an unauthenticated deployment. When enabled, every request passes through a middleware chain before reaching any handler:

```text
request
  └─▶ securityHeaders          adds CSP, X-Frame-Options, etc. (always active)
  └─▶ corsMiddleware            validates Origin against allowlist (always active)
  └─▶ requireAuth               gates routes behind session / device token (auth only)
        ├─ public paths bypass  /api/auth/login, /api/auth/refresh, /app/*, /sw.js
        ├─ /hooks/* bypasses    webhook auth is HMAC-based, not session-based
        └─ /mcp goes to requireAPIKey instead of requireAuth
```

Middleware is applied in [server.go](../../pkg/webui/server.go) inside `Handler()`.

---

## Phase 1 — Auth Wall & Security Headers

### Config

```yaml
server:
  auth: true
  secret: ""                          # optional YAML override — see passphrase source priority below
  allowed_origins: []                 # empty = same-origin only
  trust_proxy: false                  # set true when behind nginx/Caddy
```

### Passphrase source priority

The effective passphrase is resolved in this order on every auth check:

```text
1. server.secret (YAML)  — highest priority; use for headless / scripted setups
2. kv["auth.passphrase"] — stored in SQLite; managed via web UI or API
3. ""                    — bootstrap state (see auto-generation below)
```

**Auto-generation on first boot**: if `server.auth: true` and no passphrase is set (neither YAML nor DB), dicode generates a cryptographically random 43-character passphrase (32 random bytes, base64url) and prints it to stdout once:

```text
╔══════════════════════════════════════════════════════════════╗
║  dicode — auth passphrase generated                         ║
║                                                              ║
║  <43-char passphrase>                                        ║
║                                                              ║
║  Save this somewhere safe. You can change it any time at    ║
║  /security in the web UI (requires a valid session).        ║
╚══════════════════════════════════════════════════════════════╝
```

The passphrase is immediately persisted in SQLite. Subsequent restarts read it from the DB — the banner is not shown again.

**YAML override behaviour**: when `server.secret` is set, the API refuses passphrase changes (`409 Conflict`) to prevent split-brain state. Remove the YAML field to manage the passphrase via the web UI.

### `requireAuth` middleware

Defined in [auth.go](../../pkg/webui/auth.go).

```text
incoming request
  ├── is public path? → allow through
  ├── has valid session cookie? → allow through
  ├── has valid device cookie? → renewFromDevice()
  │     ├── ok → issue new session cookie, allow through
  │     │         (also sets new device cookie if token was rotated)
  │     └── fail → clear cookies, fall through to 401/redirect
  └── is API request? → 401 JSON
      else           → redirect /?auth=required
```

**Public paths** (never require auth):

- `POST /api/auth/login` — login endpoint itself
- `POST /api/auth/refresh` — silent session renewal (device cookie only, no session required)
- `/app/*` — static SPA assets (JS, CSS) needed to render the login page
- `/sw.js` — service worker
- `/hooks/*` — webhooks; auth is per-task HMAC, not global session

> **Deprecated**: `POST /api/secrets/unlock` is a legacy alias for `/api/auth/login`, kept for one release. Clients should migrate to `/api/auth/login`.

### Security headers

`securityHeaders` middleware (always active, independent of `server.auth`) adds:

| Header | Value |
| --- | --- |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `SAMEORIGIN` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=()` |
| `Content-Security-Policy` | script-src self + cdn.jsdelivr.net + esm.sh; style-src self unsafe-inline; connect-src self ws: wss: esm.sh |

### CORS

`corsMiddleware` runs before `requireAuth`. Behaviour:

- If `server.allowed_origins` is empty → no `Access-Control-Allow-Origin` header is ever sent (same-origin only)
- If an origin matches the allowlist → `ACAO: <that origin>` + `Vary: Origin` + `Access-Control-Allow-Credentials: true`
- If an origin does not match → no header (browser blocks the request)

Origins are validated with `url.Parse()` at middleware init time. Entries with no `Host` or `Scheme` are **skipped and logged** — a config typo like `"https://good.com https://evil.com"` (space instead of two list items) is ignored rather than silently corrupting the allowlist.

### X-Forwarded-For and rate limiting

`clientIP(r, trustProxy bool)` in [auth.go](../../pkg/webui/auth.go):

```go
func clientIP(r *http.Request, trustProxy bool) string {
    if trustProxy {
        // read leftmost IP from X-Forwarded-For
    }
    // fall back to r.RemoteAddr
}
```

`X-Forwarded-For` is **only trusted** when `server.trust_proxy: true`. Without this flag, a direct client could supply a spoofed header and bypass the IP-based rate limiter on the login endpoint. Set this flag only when dicode sits behind a reverse proxy that sets (and strips client-supplied) `X-Forwarded-For`.

#### Login rate limiter

`unlockLimiter` in [server.go](../../pkg/webui/server.go):

- 5 attempts per IP per minute
- On the 5th failed attempt the window is **extended to 15 minutes** (not just reset to 1 minute)
- Each IP is tracked independently — one IP being locked out does not affect others
- The map is never persisted; restarts reset all counters

```go
const (
    unlockMaxAttempts = 5
    unlockWindow      = time.Minute
    unlockLockoutTTL  = 15 * time.Minute
)
```

---

## Phase 2 — Sessions and Trusted Browser

Two cookie types are in play:

| Cookie  | Name                  | TTL      | Stored as              |
| ------- | --------------------- | -------- | ---------------------- |
| Session | `dicode_secrets_sess` | 8 hours  | In-memory map only     |
| Device  | `dicode_device`       | 30 days  | SHA-256 hash in SQLite |

Both cookies are: `HttpOnly`, `SameSite=Strict`, `Path=/`.

### Session token lifecycle

Generated by `sessionStore.issue()` in [server.go](../../pkg/webui/server.go):

```go
func (s *sessionStore) issue() string {
    raw := make([]byte, 32)  // 32 bytes = 256 bits from crypto/rand
    _, _ = rand.Read(raw)
    token := hex.EncodeToString(raw)
    s.tokens[token] = time.Now().Add(8 * time.Hour)
    return token
}
```

Key properties:

- **Purely random** — no passphrase is involved in token generation
- Validated by in-memory map lookup (`sessionStore.valid(token)`)
- Lost on restart — that is intentional; the device cookie handles persistence
- Purged hourly via `purgeLoop()`

### Device token lifecycle

Managed by `dbSessionStore` in [sessions_db.go](../../pkg/webui/sessions_db.go).

**Issuance** (at login with `trust: true`):

1. Generate 32 random bytes → hex-encode → raw token
2. Compute `sha256(raw)` → store only the hash in the `sessions` table
3. Return raw token to be placed in the `dicode_device` cookie

**Renewal** (`renewFromDevice(ctx, rawDeviceToken, ip string)`):

1. Hash the incoming cookie value
2. Open a **database transaction**
3. Query for a matching, non-expired `device` row
4. If not found → return `("", false)`
5. If found and `age < deviceRotateAfter` (24h) → update `last_seen` + `ip`, commit
6. If found and `age ≥ deviceRotateAfter` → **rotate**:
   - Generate a new raw token
   - INSERT new row (new hash, new expiry, same label)
   - DELETE old row
   - Commit transaction
   - Return `(newRawToken, true)`
7. Caller always generates a new in-memory session via `sessions.issue()` and sets it as the session cookie
8. If `newRawToken != ""` (rotation occurred), caller also calls `setDeviceCookie(w, newRawToken)`

The transaction ensures that even under concurrent logins, you never end up with both the old and new token simultaneously valid, and you never lose the device record mid-rotation.

**Storage schema** (`sessions` table):

```sql
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,          -- sha256(raw_token), never raw
    kind       TEXT NOT NULL,          -- 'device' (reserved: 'session' for future RBAC)
    label      TEXT,                   -- truncated User-Agent (≤200 chars)
    ip         TEXT,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_hash ON sessions(token_hash);
```

### Login flow

```text
POST /api/auth/login  {"password":"…","trust":true}
  │
  ├─ rate limit check (IP-based)
  ├─ resolvePassphrase() → YAML secret → DB kv["auth.passphrase"] → ""
  ├─ ConstantTimeCompare(password, resolvedPassphrase)
  ├─ sessions.issue() → 32-byte random session token → cookie
  └─ if trust: dbSessions.issueDeviceToken(ip, user-agent) → device cookie

Response: 200 {"status":"ok"}  +  Set-Cookie: dicode_secrets_sess + dicode_device
```

There is no secondary "secrets unlock" step — one login grants access to all protected resources including the secrets API.

### Silent refresh flow

```text
SPA detects 401 from api.js
  │
  ├─ POST /api/auth/refresh  (sends dicode_device cookie)
  │     ├─ dbSessions.renewFromDevice(deviceToken, ip)
  │     │     ├─ ok, no rotation → sessions.issue() → new session cookie
  │     │     └─ ok, rotated    → sessions.issue() → new session + new device cookie
  │     └─ fail → 401, clear cookies → show login modal
  └─ on success: retry original request with new session cookie
```

### Secrets API endpoints

All require a valid session. Secret **values are never returned via API** — secrets are write-only from the API's perspective and injected directly into task environment at runtime.

| Method | Path | Response |
| --- | --- | --- |
| `GET` | `/api/secrets` | `["KEY_NAME_1", "KEY_NAME_2"]` — key names only |
| `POST` | `/api/secrets` | `{status: "ok"}` — creates/updates a secret |
| `DELETE` | `/api/secrets/{key}` | `{status: "ok"}` — removes a secret |

### Device management endpoints

All require a valid session:

| Method | Path | Action |
| --- | --- | --- |
| `GET` | `/api/auth/devices` | List active trusted devices |
| `DELETE` | `/api/auth/devices/{id}` | Revoke one device |
| `POST` | `/api/auth/logout` | Revoke current session + device cookie |
| `POST` | `/api/auth/logout-all` | Wipe all in-memory sessions + all device rows |

### Passphrase management endpoints

| Method | Path | Auth | Action |
| --- | --- | --- | --- |
| `GET` | `/api/auth/passphrase` | session | Returns `{"source":"yaml"/"db"/"none"}` — never the value |
| `POST` | `/api/auth/passphrase` | session | Change the DB-stored passphrase |

`POST /api/auth/passphrase` request body:

```json
{"current": "old-passphrase", "passphrase": "new-passphrase-16chars+"}
```

Rules:

- Requires a valid session
- `current` must match the active passphrase (constant-time compare); skipped only when no passphrase is set yet (bootstrap)
- New passphrase must be ≥ 16 characters
- Blocked with `409` when `server.secret` (YAML override) is active
- On success: all in-memory sessions and DB device tokens are invalidated — everyone must re-login

---

## Phase 3 — Webhook HMAC Authentication

Configured per task in `task.yaml`:

```yaml
trigger:
  webhook: /hooks/my-task
  webhook_secret: "${MY_WEBHOOK_SECRET}"
```

When `webhook_secret` is absent the webhook is open (backwards-compatible). When set, every POST must include a valid `X-Hub-Signature-256` header.

### Signature verification

`verifyWebhookSignature(spec, r, body []byte)` in [engine.go](../../pkg/trigger/engine.go):

```text
1. If no secret configured → return nil (open webhook)
2. Check X-Dicode-Timestamp if present:
   - Parse as Unix int64
   - Reject if |now - ts| > 5 minutes  (replay protection)
3. Check X-Hub-Signature-256:
   - Reject if header is missing
   - Compute HMAC-SHA256(secret, body)
   - Expected = "sha256=" + hex(mac)
   - hmac.Equal(got, expected)  ← constant-time comparison
```

### Raw body capture

The webhook handler reads the **raw body bytes before any parsing**:

```go
if r.Body != nil {
    body, _ = io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes)) // 5 MB cap
}
// For form-encoded bodies, replay the bytes so ParseForm can read them:
if strings.Contains(ct, "application/x-www-form-urlencoded") {
    r.Body = io.NopCloser(bytes.NewReader(body))
    r.ParseForm()
}
```

This is critical: if form-encoded bodies were parsed via `r.ParseForm()` first (which consumes `r.Body`), the `body` slice would be empty and HMAC would always be computed over `[]byte{}` rather than the actual content.

### GitHub compatibility

The signature format is intentionally identical to GitHub's webhook delivery. A GitHub webhook pointed at a dicode endpoint with the same secret works with zero configuration on the GitHub side.

Constants:

```go
webhookSignatureHeader    = "X-Hub-Signature-256"
webhookTimestampHeader    = "X-Dicode-Timestamp"
webhookTimestampTolerance = 5 * time.Minute
webhookMaxBodyBytes       = 5 << 20  // 5 MB
```

---

## Phase 4 — MCP API Key Authentication

### Key format

`dck_<64 hex chars>` — 68 characters total.

- `dck_` prefix: greppable, distinguishable from other secrets
- 64 hex chars = 32 random bytes from `crypto/rand` = 256 bits of entropy
- Not guessable, not derivable from any other value

### Storage

```sql
CREATE TABLE IF NOT EXISTS api_keys (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    key_hash   TEXT NOT NULL,   -- sha256(raw_key), NEVER the raw key
    prefix     TEXT NOT NULL,   -- first 12 chars + "..." for display
    created_at INTEGER NOT NULL,
    last_used  INTEGER,
    expires_at INTEGER          -- NULL = no expiry
);
```

The raw key is returned **once** at creation and never stored. If lost, create a new key.

### Key generation

`apiKeyStore.generate(ctx, name)` in [apikeys.go](../../pkg/webui/apikeys.go):

```go
rawBytes, _ := randomToken()          // 32 crypto/rand bytes, hex-encoded
raw = "dck_" + rawBytes               // 68-char key
hash = sha256(raw)                    // stored
prefix = raw[:12] + "..."             // displayed (dck_XXXXXXXX...)
```

The prefix guard ensures `len(raw) >= 12` before slicing (avoids panic on hypothetical key format changes).

### Validation

`apiKeyStore.validate(ctx, raw)`:

1. `strings.HasPrefix(raw, "dck_")` — fast reject
2. `hashAPIKey(raw)` — compute SHA-256
3. Query DB: `WHERE key_hash = ? AND (expires_at IS NULL OR expires_at > ?)`
4. If found: update `last_used`, return `true`

### Middleware

`requireAPIKey` in [apikeys.go](../../pkg/webui/apikeys.go) — mounted on `/mcp`:

```go
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !s.cfg.Server.Auth { // no-op when auth disabled
            next.ServeHTTP(w, r)
            return
        }
        raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
        if raw == "" || !s.apiKeys.validate(r.Context(), raw) {
            w.Header().Set("WWW-Authenticate", `Bearer realm="dicode"`)
            jsonErr(w, "invalid or missing API key", 401)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

### Key management endpoints

All require a valid session (not just an API key — key management is a human operation):

| Method | Path | Response |
| --- | --- | --- |
| `GET` | `/api/auth/keys` | List of `APIKeyInfo` (no raw values) |
| `POST` | `/api/auth/keys` | `{key: "dck_…", info: APIKeyInfo}` — key shown once |
| `DELETE` | `/api/auth/keys/{id}` | `{status: "revoked"}` |

---

## Database Schema Summary

Both tables are created in the SQLite migration in `pkg/db/sqlite.go`:

```sql
-- Trusted device tokens (Phase 2)
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,
    kind       TEXT NOT NULL DEFAULT 'device',
    label      TEXT,
    ip         TEXT,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_hash ON sessions(token_hash);

-- MCP / programmatic API keys (Phase 4)
CREATE TABLE IF NOT EXISTS api_keys (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    key_hash   TEXT NOT NULL UNIQUE,
    prefix     TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_used  INTEGER,
    expires_at INTEGER
);
```

Expired rows are cleaned up by `dbSessionStore.purgeExpired(ctx)` which is called on a schedule from the server.

---

## Configuration Reference

All security-relevant fields in `ServerConfig`:

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `auth` | bool | `false` | Enable global auth wall |
| `secret` | string | `""` | YAML passphrase override — highest priority; if omitted dicode auto-generates one on first boot and stores it in SQLite |
| `allowed_origins` | []string | `[]` | CORS allowlist — empty = same-origin only |
| `trust_proxy` | bool | `false` | Trust `X-Forwarded-For` (set when behind a reverse proxy) |
| `mcp` | bool | `true` | Expose MCP endpoint at `/mcp` |

---

## Adding a New Protected Endpoint

1. Mount it inside the auth-gated group in `Handler()` in `server.go` (not in the public group)
2. The `requireAuth` middleware will automatically protect it
3. If the endpoint is for MCP/agent use, mount it under `/mcp` and it will be protected by `requireAPIKey`
4. If the endpoint manages security-sensitive resources (keys, devices), add a `!s.authSessionValid(r)` check to ensure only session-authenticated users (not just API-key authenticated ones) can access it

---

## Security Properties Summary

| Property | Implementation |
| --- | --- |
| Session tokens are random | `crypto/rand`, 32 bytes, never passphrase-derived |
| Device tokens stored as hash | SHA-256 in SQLite, raw value only in cookie |
| API keys stored as hash | SHA-256 in SQLite, raw value returned once |
| Password comparison is constant-time | `crypto/subtle.ConstantTimeCompare` |
| Webhook signatures are constant-time | `hmac.Equal` |
| CSRF protection | `SameSite=Strict` on all cookies |
| Clickjacking protection | `X-Frame-Options: SAMEORIGIN` |
| MIME sniffing protection | `X-Content-Type-Options: nosniff` |
| Device token rotation | Atomic DB transaction, old token deleted |
| IP spoofing guard | XFF only trusted with explicit `trust_proxy: true` |
| Brute force protection | 5 attempts/IP, then 15-min lockout |
| Replay attack protection | 5-minute timestamp window on signed webhooks |
| CORS misconfiguration guard | Origins validated with `url.Parse()` at startup |
| Passphrase rotation requires current | `crypto/subtle.ConstantTimeCompare` on `current` field |
