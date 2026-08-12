# Security — Developer Reference

This document covers the full security architecture implemented across `pkg/webui/auth.go`, `pkg/webui/scsstore.go`, `pkg/webui/sessions_db.go`, `pkg/webui/apikeys.go`, `pkg/webui/server.go`, and `pkg/trigger/engine.go`. It is intended for contributors modifying the auth system and for operators who need to understand the trust model.

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
  device_binding: off                 # off | warn | strict — bind trusted-device cookie to IP subnet + UA family
```

### Passphrase source priority

The effective passphrase is resolved in this order on every auth check:

```text
1. server.secret (YAML)  — highest priority; use for headless / scripted setups
2. kv["auth.passphrase"] — stored in SQLite; managed via web UI or API
3. ""                    — bootstrap state (see auto-generation below)
```

### Passphrase storage — bcrypt hashing

The DB-backed passphrase (source #2 above) is stored as a **bcrypt hash**, not
plaintext. The work factor defaults to 12 (~300ms per hash on 2024 server
hardware) and is tunable via:

```yaml
server:
  bcrypt_cost: 12          # default; valid range 4–14
```

Lower the cost on resource-constrained devices (e.g. `bcrypt_cost: 10` on a Pi
Zero) where ~300ms per login is too slow; raise it on beefy boxes that can
afford >1s logins. Values outside 4–14 are rejected at config-load time —
bcrypt accepts up to 31, but anything above ~14 is multi-second and offers
no practical security gain for a single-user passphrase while risking
operator self-lockout from a typo'd config.

**bcrypt 72-byte caveat**: bcrypt silently truncates passwords longer than
72 bytes. To prevent operators from believing their 100-character passphrase
is fully protecting them, `POST /api/auth/passphrase` rejects inputs longer
than 72 bytes with `400 Bad Request`. UTF-8 length is byte-counted — a few
emoji can blow this fast.

**Lazy migration from legacy plaintext**: deployments that pre-date this
change have a plaintext value in `kv["auth.passphrase"]`. The shape is
detected by `looksLikeBcryptHash` (`$2a$`, `$2b$`, `$2y$`, `$2$` prefixes);
anything else is treated as legacy plaintext. On the next **successful**
login the value is rehashed and overwritten — no operator action required,
no passphrase reset, no downtime.

Migration semantics:

- Failed login attempts never trigger a rehash (the legacy plaintext is the
  authoritative source until a real success).
- Rehash failures are logged at WARN and swallowed — the operator's already-
  authenticated session is not denied because of a transient DB issue. The
  next successful login will retry.
- Concurrent successful logins on the same legacy plaintext are collapsed by
  a `singleflight.Group` so exactly one bcrypt computation + DB write
  happens per migration event, no matter how many goroutines race in.
- The in-process cache (`Server.cachedPassphrase`) is warmed with the new
  hash so subsequent verifications skip both the legacy branch and the DB
  read.

**Fail-closed on DB read errors**: a transient DB outage (or context
deadline, or anything else that makes the `kv` query error) causes
`passphraseSource()` to return `passphraseSourceUnknown` rather than
`passphraseSourceNone`. This is load-bearing: the bootstrap fast-path in
`apiSecretsUnlock` accepts any password when no passphrase is set yet, so
silently returning "none" on an error would let any login through during
an outage. The login and passphrase-change endpoints both translate
`passphraseSourceUnknown` → `503 Service Unavailable`.

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

### Security headers

`securityHeaders` middleware (always active, independent of `server.auth`) adds:

| Header | Value |
| --- | --- |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `SAMEORIGIN` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=()` |
| `Content-Security-Policy` | script-src self + cdn.jsdelivr.net + esm.sh; style-src self unsafe-inline + cdn.jsdelivr.net; connect-src self ws: wss: esm.sh + cdn.jsdelivr.net; font-src self + cdn.jsdelivr.net |

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

`go-chi/httprate` middleware in [server.go](../../pkg/webui/server.go):

- 5 attempts per IP per minute (flat rate limit via `httprate.Limit`)
- Each IP is tracked independently — one IP being locked out does not affect others
- Counters are in-memory; restarts reset them

```go
const (
    unlockMaxAttempts = 5
    unlockWindow      = time.Minute
)
```

---

## Phase 2 — Sessions and Trusted Browser

Two cookie types are in play:

| Cookie  | Name                  | TTL      | Stored as              |
| ------- | --------------------- | -------- | ---------------------- |
| Session | `dicode_secrets_sess` | 8 hours  | SQLite via scs/v2      |
| Device  | `dicode_device`       | 30 days  | SHA-256 hash in SQLite |

Both cookies are: `HttpOnly`, `SameSite=Strict`, `Path=/`.

### Session token lifecycle

Short-lived sessions are managed by [`alexedwards/scs/v2`](https://github.com/alexedwards/scs) backed by a SQLite store adapter (`pkg/webui/scsstore.go`). The scs library handles token generation, cookie management, and automatic expiry.

Key properties:

- **Purely random** — scs generates cryptographically random session tokens; no passphrase is involved
- Validated via the SQLite-backed scs store (sessions survive restarts)
- Expired sessions are cleaned up automatically by scs

### Device token lifecycle

Managed by `dbSessionStore` in [sessions_db.go](../../pkg/webui/sessions_db.go).

**Issuance** (at login with `trust: true`):

1. Generate 32 random bytes → hex-encode → raw token
2. Compute `sha256(raw)` → store only the hash in the `sessions` table
3. Return raw token to be placed in the `dicode_device` cookie

**Renewal** (`renewFromDevice(ctx, rawDeviceToken, ip, userAgent, mode string)`):

1. Hash the incoming cookie value
2. Open a **database transaction**
3. Query for a matching, non-expired `device` row
4. If not found → return `("", false)`
5. Evaluate device binding (`mode` = `server.device_binding`) — see below. In `strict` mode a drift hard-revokes the device row (`DELETE` in the same transaction) and returns the reject path; every caller then destroys the session and clears the `dicode_device` cookie, so a cookie judged stolen cannot be replayed until its 30-day expiry. In `warn` mode the drift reason is recorded on the row and a structured event is logged.
6. If found and `age < deviceRotateAfter` (24h) → update `last_seen` + `ip` + `ua_family`, commit
7. If found and `age ≥ deviceRotateAfter` → **rotate**:
   - Generate a new raw token
   - INSERT new row (new hash, new expiry, same label)
   - DELETE old row
   - Commit transaction
   - Return `(newRawToken, true)`
8. Caller always generates a new in-memory session via `sessions.issue()` and sets it as the session cookie
9. If `newRawToken != ""` (rotation occurred), caller also calls `setDeviceCookie(w, newRawToken)`

The transaction ensures that even under concurrent logins, you never end up with both the old and new token simultaneously valid, and you never lose the device record mid-rotation.

### Device binding (`server.device_binding`)

A trusted-device cookie is a 30-day bearer credential. To limit the blast radius of a stolen cookie, the issuing **IP subnet** and **User-Agent family** are recorded and can be verified on each renewal:

| Mode     | Behaviour |
| -------- | --------- |
| `off`    | Default. IP/UA are recorded but never verified — backward-compatible. |
| `warn`   | Renewal is allowed even on drift, but the drift is flagged (`drift: true` + reason) on the `/security` device row and a structured `device.binding_drift` log event is emitted. The flag is **sticky**: it is compared against the issue-time baseline (the stored IP/UA family is not re-anchored to the drifted client), so a persistent drift keeps showing until the client genuinely returns to its issuing subnet/family. |
| `strict` | Renewal is **rejected** on drift, the offending device row is **hard-revoked** in the same transaction, and the session is destroyed and the device cookie cleared on every code path (`requireAuth`, `hasValidSession`, `/api/auth/refresh`), forcing a fresh login. |

"Drift" means either:

- **IP subnet change** — compared at **/24 for IPv4** and **/48 for IPv6**, deliberately coarse so mobile NAT and carrier IP churn within a network do not trip the binding. Full-IP comparison is intentionally *not* used.
- **UA-family mismatch** — the User-Agent is reduced to a coarse `browser/os` family (e.g. `chrome/macos`), dropping version numbers so routine browser auto-updates don't force a re-login. A cookie replayed from a different browser or OS is caught.

> **Caveat:** UA-family binding only catches *accidental* cross-browser cookie copy (e.g. a token pasted into a different browser). A deliberate attacker can trivially set the victim's User-Agent header, so this is a defence-in-depth signal, not a primary control — the IP-subnet check and the short session TTL carry the real weight.

**UA family** is stored in the `ua_family` column. Rows issued before this feature have `ua_family = NULL`; a NULL family is **never** treated as a mismatch — the current family is backfilled on the next renewal. This keeps existing trusted devices working when an operator first enables binding.

The passphrase-rotation kill-switch (`pkg/webui/passphrase.go`) still revokes **all** device tokens, independent of binding mode.

**Storage schema** (`sessions` table):

```sql
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,          -- sha256(raw_token), never raw
    kind       TEXT NOT NULL,          -- 'device' (reserved: 'session' for future RBAC)
    label       TEXT,                  -- truncated User-Agent (≤200 chars)
    ip          TEXT,
    ua_family    TEXT,                  -- coarse browser/os family; NULL on legacy rows
    drift_reason TEXT NOT NULL DEFAULT '', -- last warn-mode binding drift: '', 'ip', 'ua', 'ip+ua'
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

### Login page — passphrase-required signal

`GET /api/login/context` (unauthenticated, fetched by `pkg/webui/login/login.js`) returns:

```json
{"title": "Sign in to dicode", "passphrase_required": true}
```

`passphrase_required` is `false` only when `passphraseSource()` reports `passphraseSourceNone` — i.e. `apiSecretsUnlock` will accept **any** password, including empty. In practice that's `server.auth: false`, the common default: `ensurePassphrase` never generates a passphrase when auth is disabled, so `/login` (always publicly reachable, independent of `server.auth`) would otherwise still present what looks like a real credential gate. When auth *is* enabled, `ensurePassphrase` runs synchronously in `Start()` before the HTTP listener accepts any connection, so a passphrase always exists by the time this endpoint is reachable — there is no first-boot window where `passphraseSourceNone` is observable over HTTP with auth on.

`passphraseSourceUnknown` (transient DB read failure) reports `passphrase_required: true` — login is fail-closed (`503`) in that state, so the page must keep looking like a real gate.

When `passphrase_required` is `false`, the login page removes the password field's `required` attribute and shows an inline notice ("No password is configured for this dicode instance") instead of silently accepting whatever the operator happens to type, which previously implied a real check was happening.

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

`verifyWebhookSignature(spec, r, body []byte) (tsStr string, digest []byte, err error)` in
[webhook.go](../../pkg/trigger/webhook.go):

```text
1. If no secret configured → return ("", nil, nil) (open webhook)
2. If X-Dicode-Timestamp is absent:
   - Reject if trigger.require_timestamp is set (default false)
   - Otherwise check X-Hub-Signature-256 against HMAC-SHA256(secret, body)
3. If X-Dicode-Timestamp is present:
   - Parse as Unix int64
   - Reject if |now - ts| > 5 minutes  (replay protection)
   - Check X-Hub-Signature-256 against HMAC-SHA256(secret, "<ts>\n<body>")
   - hmac.Equal(got, expected)  ← constant-time comparison
   - Return the raw timestamp string and the computed HMAC digest so the
     caller can bind both into the replay-cache key without a second HMAC
     pass over the body
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

### Replay protection (nonce cache)

Even with timestamp validation, an attacker can replay a captured request
within the 5-minute tolerance window. To close this gap, dicode maintains a
bounded in-memory nonce cache keyed on `HMAC(secret, "<ts>\n<body>")` when
the request carried a timestamp, or `HMAC(secret, body)` when it didn't —
the same preimage the signature itself covers, so the cache key changes
exactly when the signed content changes.

When a signed webhook arrives:

1. HMAC signature is verified (unchanged).
2. Timestamp is checked if `X-Dicode-Timestamp` is present (unchanged); the
   verified timestamp string is threaded into the replay check.
3. The HMAC digest — computed over `(ts, body)` when a timestamp was sent, or
   `body` alone otherwise — is looked up in the nonce cache:
   - **Already seen (within 1 hour)** → HTTP 409 Conflict.
   - **Not seen** → request accepted, digest recorded.

Keying on `(ts, body)` rather than `body` alone fixes a real gap in the
body-only key: two legitimate requests with an identical body at different
timestamps no longer collide as a false replay. It does **not**, by itself,
change daemon-restart exposure — the cache is purely in-memory (see below)
and starts empty after every restart regardless of key shape, so a captured
request that is still inside its own timestamp tolerance window is
re-admitted after a restart under either keying scheme. What actually bounds
that exposure is the timestamp-tolerance check itself, which runs
independently of the cache and rejects any timestamp older than 5 minutes;
`require_timestamp` (below) extends that bound to senders who would
otherwise send no timestamp — and therefore have no expiry — at all.

The cache is bounded to 10,000 entries with FIFO eviction. Entries older
than 1 hour are lazily pruned. The cache lives in-memory on the Engine
instance — a daemon restart clears it. That's only a bounded exposure when a
timestamp is present: a captured request whose timestamp has since aged past
the 5-minute tolerance is rejected regardless of the cache being empty.
Without `require_timestamp`, a sender that never sends a timestamp has no
such bound — its signature stays valid indefinitely — so a restart (or
simply outliving the 1-hour cache TTL) re-admits it exactly as it did before
this change.

Replay protection is enabled by default when `webhook_secret` is set.
Per-task opt-out via `replay_protection: false` in `task.yaml`.

### Mandatory timestamp (`require_timestamp`)

`X-Dicode-Timestamp` is optional by default so senders that sign only the
body — GitHub's webhook delivery, notably — keep working unmodified (see
[GitHub compatibility](#github-compatibility) above). The tradeoff: a sender
that never sends a timestamp is replayable indefinitely once its request
ages out of the 1-hour nonce cache or the daemon restarts, since a fixed
body always hashes to the same digest.

Set `require_timestamp: true` in `trigger` to close that gap by rejecting any
request that omits `X-Dicode-Timestamp` outright — recommended for any
webhook exposed through the relay tunnel, where requests already cross a
public network boundary before reaching the daemon:

```yaml
trigger:
  webhook: /hooks/my-task
  webhook_secret: "${MY_WEBHOOK_SECRET}"
  require_timestamp: true
```

`require_timestamp` defaults to `false` (unset) and has no effect on
requests when `webhook_secret` is absent (open webhooks are unaffected).

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

## Audit Log

A structured audit log records security-sensitive operations at the system's
trust boundaries (`pkg/audit`). Events are appended to the `audit_log` table
(below) and surfaced via `GET /api/audit`.

**Emission boundaries:**

| Event | Where | Captures |
| --- | --- | --- |
| `run_triggered` | `trigger.Engine.startRun()` | every run start (cron/webhook/manual/chain/daemon/replay); the manual/API actor is recorded as the client IP |
| `task_called` | `pkg/ipc` `dicode.run_task` | a task invoking another task — allowed **and** denied (capability / `allowed_tasks` / MCP-exposure) |
| `mcp_called` | `pkg/ipc` | the `buildin/mcp` `tools/call` forwarder and outbound `mcp.call` to external MCP servers |
| `denied` | `pkg/webui` | rejected auth (`requireAuth`, API-key, webhook guard, failed passphrase login) |

**Redaction:** `params` is stored as JSON with values replaced by `[REDACTED]`
when the key name matches the secret deny-list (mirroring
`pkg/registry/inputredact.go`) or the value is an `env:` / `secret:` /
`secrets:` reference. Nested MCP arguments are walked recursively, so audit
rows never contain secret values.

**Retention:** `audit_log.retention_days` (top-level config) controls pruning —
unset defaults to **30** days; an explicit `0` disables pruning. The daemon
prunes once at startup and every 6 hours.

Writes are best-effort: a failed audit insert never breaks or blocks the
operation being audited. The log is queryable via `GET /api/audit` (see the
[Web UI API](webui-api.md) reference), behind `requireAuth` like every other
API route.

---

## Container Security Floor

Docker and podman tasks accept host configuration from untrusted `task.yaml`
files. `pkg/runtime/containersec` enforces a **default-deny floor**: dangerous
host config is rejected (the run is aborted before the container is created) in
**both** runtimes unless an operator explicitly opts in. The zero-value policy
denies everything dangerous, so the floor is fail-closed even if unconfigured.

**Rejected by default:**

- `network_mode: host` (and `container:<id>`, `ns:<path>`)
- `cap_add` of escape-enabling capabilities: `ALL`, `SYS_ADMIN`, `SYS_PTRACE`,
  `SYS_MODULE`, `SYS_RAWIO`, `SYS_BOOT`, `SYS_TIME`, `NET_ADMIN`,
  `DAC_READ_SEARCH`, `DAC_OVERRIDE`, `BPF`, `SYSLOG` (case- and `CAP_`-prefix
  insensitive)
- `security_opt` that disables a sandbox layer: `seccomp=unconfined`,
  `apparmor=unconfined`, `label=disable`, `systempaths=unconfined`, `unmask=…`
- bind-mount sources resolving (after `..`/symlink cleaning) to `/` or under
  `/proc`, `/sys`, `/etc`, `/dev`, `/boot`, `/root`, `/run`, `/var/run`,
  `/var/lib/docker`, `/var/lib/containers`; the docker/podman control sockets
  (by subtree **and** basename); and all relative sources. Named/anonymous
  volumes remain allowed.

**Operator opt-out** — the top-level `container_security` block in `dicode.yaml`:

```yaml
container_security:
  allow_host_network: false          # permit network_mode: host / container: / ns:
  allow_insecure_security_opt: false # permit seccomp/apparmor/label/systempaths/unmask
  allowed_cap_add: []                # capabilities tasks may add (["ALL"] = any)
  allowed_volume_roots: []           # absolute roots; when set, strict allowlist mode
```

When `allowed_volume_roots` is non-empty the policy switches to **strict
allowlist mode**: every bind-mount source must resolve inside one of the listed
(absolute) roots, and an explicitly listed root overrides the built-in denylist.

Podman additionally validates task-controlled argv values (image refs / flag
values starting with `-`, control characters, env-key smuggling) before
invoking the CLI. Orphaned `dicode-*` build images are reclaimed by a GC pass.

---

## Approval Lock Integrity (HMAC-signed dicode.lock)

The trust-on-change gate persists approval records in `dicode.lock`. Because
this file controls which task versions are allowed to run, its integrity is
security-critical. Starting from format version 2 each lock file is
cryptographically signed with an HMAC key derived from the daemon's master key.

### Threat model

| Vector | Prior defense | Residual before v2 |
|--------|---------------|---------------------|
| In-place overwrite via broad `fs: w` | `--deny-write` (PR #431) | Stopped |
| Symlink write-through bypass | Python guard resolves realpath; Deno lexical check | Deno symlink still allowed |
| Delete + replace (swap attack) | Fail-closed bootstrap + DB marker | Content not integrity-checked |
| Forged content from DB wipe + lock tamper | DB marker; secrets store wipe loud | No cryptographic integrity |

### Signing scheme

```plaintext
lockSigningKey = DeriveSubKey(masterKey, "dicode/approval-lock/v1")  // Argon2id
canonicalContent = JSON({bootstrapped, tasks_map})                    // Go sort-ordered (v3)
mac = hex(HMAC-SHA256(lockSigningKey, canonicalContent))             // stored in mac: field
```

The key is derived from the master key (`DICODE_MASTER_KEY` / `~/.dicode/master.key`),
which is never forwarded to task subprocesses and survives independently of the
SQLite database. Even if a task wipes the database it cannot forge a valid MAC
without the master key.

### Bootstrap marker — dual-store defence (#434)

The first-run bootstrap marker is persisted in **two independent locations**:

1. **`dicode.lock` `bootstrapped:` field** (v3 format) — covered by the HMAC,
   so it cannot be silently flipped without invalidating the MAC. Survives SQLite
   deletion.
2. **`kv["approval.bootstrap_completed"]` row** in SQLite — survives `dicode.lock`
   deletion (directory-level attacks that the file-level deny cannot block).

At daemon startup the effective marker is:

```
markerExists = lock.IsBootstrapped() || dbMarkerExists
```

**Both** stores must be absent for bootstrap to re-run. Deleting either one alone
is insufficient to reset the gate.

### Load-time behaviour

| Condition | Outcome |
|-----------|---------|
| File absent | Empty lock (normal first-run) |
| Legacy v1 (no `mac:` field), key available | Accepted + immediately upgraded to v3 (format upgrade) |
| v2 `mac:` (tasks-only HMAC), verification passes | Accepted + immediately upgraded to v3 in-place |
| v2 `mac:`, verification fails | **Fail closed**: all records discarded; `Tampered()` returns `true` |
| v3 `mac:` (bootstrapped+tasks HMAC), verification passes | Normal load |
| v3 `mac:`, verification fails | **Fail closed**: all records discarded; `Tampered()` returns `true`; daemon logs a warning; all tasks require explicit re-approval |
| Unknown version with `mac:` present | **Fail closed**: cannot verify; treated as tampered |
| No signing key available (env stripped of master key) | Unsigned/legacy mode — no verification |

### On-disk format

Signed locks use `version: 3` and include a `mac:` field directly after the
version line, before `bootstrapped:` and `tasks:`:

```yaml
# dicode.lock — approval records, managed by the dicode daemon.
version: 3
mac: 8a3f1c...  # 64 hex chars = HMAC-SHA256 over {bootstrapped, tasks}
bootstrapped: true
tasks:
  my-task:
    hash: sha256:...
    approved_at: 2026-06-01T12:00:00Z
    approved_by: manual
    commit: 4f2b9c8e...   # 40-hex; absent for a source with no git history
```

`commit:` is the git commit the approved content was observed at, captured when
the task is held pending rather than when the operator clicks approve, so it can
never describe a different generation than `hash:` does. It is decoration — the
review surface uses it to show what moved since the last approval — and no gate
decision reads it, so a source outside any repository simply records none.

Records written before `commit:` existed keep verifying: the field is
`omitempty`, so an absent commit contributes nothing to the MAC payload and
those records still hash to the bytes they were signed over.

The `bootstrapped: true` field is omitted (YAML `omitempty`) until
`MarkBootstrapped()` is called. Legacy unsigned files (`version: 1`, no `mac:`)
and v2 signed files are accepted on the first startup with a key and immediately
upgraded to v3. After that, any subsequent load that sees a missing or wrong MAC
treats the file as tampered.

---

## Pending-Change Diff (#604)

Before this feature the operator approved a pending (content-changed) task
**blind**: `dicode.lock` only ever stores a content hash, never file bytes, so
by the time a task shows up pending, the working tree already holds the *new*
content and there is no "before" snapshot on disk to diff against. `pkg/approval.Gate`
now keeps an in-memory snapshot cache to close that gap, and the WebUI surfaces
it wherever an operator can click Approve.

### Snapshot cache

`Gate` maintains one additional map, guarded by the same mutex as `pending`:

- `approvedFiles[taskID]` — the last-known-approved content snapshot (dir-relative
  path → file text), refreshed whenever `Admit` treats the current on-disk dir as
  already-approved content (the already-approved-hash fast path, and every
  auto-approve path: builtin / trusted-source / trusted-task / gate-disabled /
  bootstrap).

The current pending (not yet approved) content snapshot lives alongside the
task and its observed hash on `pendingEntry.files`, itself a value in the
existing `pending[taskID]` map — not a standalone map. Keeping hash and files
on the same struct, written together in one critical section, means a reader
can never observe a hash paired with a snapshot from a different generation
(the fix for a race where `approve()` could promote a snapshot that was stale
relative to the pending hash it was being matched against). On a successful
`Approve` / `ApproveIfHash`, `pendingEntry.files` is promoted to
`approvedFiles[taskID]` and the pending entry is deleted — becoming the new
baseline for the *next* change. `Forget` (task removal) clears both the
pending entry and `approvedFiles[taskID]`.

Each snapshot is built by `snapshotDir` (`pkg/approval/snapshot.go`), which walks
the task directory the same way `task.Hash`'s walker does (skipping
`node_modules` and `.git`). To keep this cheap enough to run on every `Admit` —
including every ~30s reconcile poll — each snapshot is bounded: at most 256 KiB
read per file and 200 files per task. A file over either cap, or one that fails
UTF-8 validation (binary), gets a placeholder entry ("binary or file too large
to diff") instead of its raw bytes, so the file still shows up as changed even
when its content can't be rendered.

**Literal secret values are redacted before storage.** `task.EnvEntry.Value`
(`pkg/task/spec.go`) lets a task's `permissions.env` block carry a literal
secret value inline in `task.yaml`, as opposed to `Secret` (a secrets-store key
*name* reference). `task.EnvEntry.Default` carries the same class of material — `envresolve`
injects it as the variable's value whenever the named secret is absent from
the store — so it is redacted identically. `snapshotDir` runs every captured
text file through `redactValueLines`, which blanks the scalar half of any line
matching a YAML `value:` or `default:` mapping entry
(`^([ \t]*(?:-[ \t]*)?"?(?:value|default)"?[ \t]*:)[ \t]*(.*)$`, multiline) to the same `<redacted>` placeholder (`redactedEnvValue`) that
`ContentHash`'s `sanitizePermissions` already uses for the content hash — see
that doc comment in `pkg/approval/gate.go` for why a literal env value must
never appear in a low-entropy, offline-attackable form. The `key:` prefix and
line structure are kept intact, so a diff still shows *that* the field
changed, never *what* it changed to/from. This happens once, at snapshot read
time — `Gate.approvedFiles` / `pendingFiles` never hold the un-redacted bytes,
so the literal cannot reach `Gate.Diff`'s output on any surface (the REST
endpoint, the dashboard panel, or the unauthenticated `/approve/{token}`
confirm page). The redaction is deliberately generic — any YAML `value:` line
in any snapshotted file, not only ones provably inside `permissions.env` —
since the snapshot has no field-path-aware YAML parse and erring toward
over-redaction is the right tradeoff for a security fix.

**This cache is in-memory only** — like the gate's `pending`/`admitted` maps, it
is rebuilt by re-`Admit` on daemon restart, not persisted to disk. A diff
requested immediately after a fresh daemon start, before the reconciler has
re-admitted a task at least once, has no cached baseline: `Diff.HasBaseline` is
`false`, and the pending files are reported as `"added"` instead of `"modified"`
so the UI still shows *something* useful — just without a real "before" to
compare against. This is a deliberate, documented tradeoff, not a bug: solving
restart-persistence for the diff cache is out of scope for this change.

### What the diff shows

`Gate.Diff(taskID)` returns a `Diff{ TaskID, HasBaseline, Files []FileDiff }`.
Each `FileDiff` is `{ Path, Status, UnifiedDiff, SecurityRelevant, OldContent,
NewContent }` — only changed files are included (`Status` is `"added"`,
`"removed"`, or `"modified"`; unchanged files are omitted entirely).
`UnifiedDiff` is a readable ` `/`-`/`+` prefixed rendering (line-mode diff via
`github.com/sergi/go-diff/diffmatchpatch`), not byte-perfect POSIX unified diff
format — just clear text for a human to scan before clicking Approve.

**Hunking.** `UnifiedDiff` keeps `diffContextLines` (3) of context either side
of each change and collapses longer runs into a `⋯ N unchanged lines` marker.
Without this the entire file ships as context: a one-line edit to a 3,000-line
task rendered 3,005 lines and a 56,000px page, and ten such files came to
1.2 MB — burying the change the diff exists to surface. The marker matches none
of the ` `/`-`/`+` prefixes, so both renderers already class it as a note
without special-casing elision. Security-relevant flagging runs against the
*full* diff before hunking, so elision can never hide a flagged line from the
check — only from the rendering.

`OldContent`/`NewContent` are the two sides of those hunks reconstructed as
plain text, for a client-side viewer to render itself. They are deliberately
derived from the hunked diff rather than the whole file: shipping both full
sides costs *more* than the unhunked diff it replaced (measured 1.2 MB → 2.3 MB
across ten changed files, versus 15 KB hunked). Both are omitted when either
side is a placeholder or would exceed `maxInlineContentBytes` (128 KiB), which
is only reachable when a file changes nearly end-to-end; a client must fall
back to `UnifiedDiff` when they are absent.

Two surfaces expose it:

- `GET /api/tasks/{id}/pending-diff` — same auth group as
  `POST /api/tasks/{id}/approve` (session cookie or non-ephemeral API key).
  `200` with the `Diff` body, `404` unknown task, `409` task not pending,
  `503` gate not wired. The dashboard's task-detail page opens this panel by
  default for a pending task and mounts a Monaco diff editor per file from
  `OldContent`/`NewContent` (virtualized, syntax-highlighted, folds unchanged
  regions), falling back to the `UnifiedDiff` text rendering when the sides are
  absent. Approve is only offered while the panel is open — collapsing it
  reverts the button to re-opening the diff, and the task list has no approve
  action at all, only a "Review" hand-off to this page.

  `Diff.pending_hash` carries the exact content hash these `Files` describe.
  The dashboard's Approve click sends it back in the request body
  (`POST /api/tasks/{id}/approve {"hash": "..."}`), and the handler routes a
  hash-carrying request through `Gate.ApproveIfHash` instead of the
  unconditional `Approve` — the same binding the tokenized `/approve/{token}`
  link has always used (#392), now also covering the session/API-key path
  (#645). Between the diff loading and the click landing, a push can re-pend
  the task at a newer hash — the panel does not poll or listen for that — so
  without this binding the click would silently arm whatever is *currently*
  pending, not the version the operator reviewed. A mismatch responds `409`
  with `{"stale": true}`; the dashboard shows that as "this task changed
  since you loaded this diff" and re-fetches the panel to the version
  actually pending, rather than retrying the same approval. Callers with no
  diff to bind to — `dicode task approve` goes over the control-socket IPC,
  not this REST endpoint, so it is unaffected — may omit `hash` and keep the
  prior unconditional-approve behavior.
- The tokenized `/approve/{token}` link page — no session, the token itself is
  the auth boundary. It fetches the same `Diff` server-side and renders it into
  the bare HTML/CSS confirm page (no JS, per that page's existing constraint),
  with colored +/− lines and a "no baseline" notice when `HasBaseline` is false.
  Monaco cannot serve this page, so the hunked `UnifiedDiff` text is what keeps
  it readable for large changes.

Dir-less (inline taskset) tasks have no files to snapshot — `Diff` returns
gracefully (`Files` empty, no error) rather than treating the absence of a
task directory as a failure.

### Security-relevant highlighting

Flagging combines two checks, both run against the *full* diff before hunking
elides context. `securityFieldPattern` matches a changed line that itself names
a security key (`permissions:`, `net:`, `cron:`, …), which catches a block being
added. `touchesSecurityBlock` tracks YAML indentation to flag a changed line
*anywhere inside* a `permissions:` or `trigger:` block, which catches one being
**widened** — appending a host to an already-approved `net:` allowlist changes
only a list item and names no key, so the pattern alone misses it. Widening an
existing block is both the likelier change under trust-on-change and the
stealthier one, so it must flag too. `touchesSecurityBlock` depends on the
pre-hunk ordering directly: elided context would drop the `permissions:` opener
that puts a changed line in scope.

A `FileDiff` is flagged `SecurityRelevant: true` when an added or removed line
in its `UnifiedDiff` touches one of the YAML keys folded into the approval
gate's content hash (see `ContentHash`'s doc comment in `pkg/approval/gate.go`
for the authoritative list this mirrors):

`permissions`, `env`, `run`, `net`, `fs`, `sys`, `dicode`, `git_commit_push`,
`webhook`, `webhook_auth`, `cron`, `manual`, `daemon`, `chain` — each matched
only when immediately followed (after optional whitespace) by a colon, so a
substring hit like `environment:` or `blockchain:` does not false-positive.

Both the dashboard and the token-link confirm page show a highlighted warning
banner ("This change touches security-relevant fields…") whenever any file in
the diff is flagged, drawing the operator's eye to exactly the changes that
could widen what the task can touch or how/whether it fires — permission
grants, env var wiring, or a trigger rewire (e.g. a manual task quietly turned
into an unauthenticated webhook) — without requiring them to read every line
of every file.

**Out of scope for this change:** `dicode task approve` (the CLI) does not
show a diff — the issue explicitly marked this "ideally," not required, to
keep the change's surface area small. The WebUI approve button, the
task-detail pending banner, and the tokenized approve-link page are covered;
a CLI diff view can follow later if wanted.

---

## Database Schema Summary

These tables are created in the SQLite migration in `pkg/db/sqlite.go`:

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

-- Structured security audit log (#45)
CREATE TABLE IF NOT EXISTS audit_log (
    id          TEXT PRIMARY KEY,
    ts          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    event_type  TEXT NOT NULL,   -- run_triggered | task_called | mcp_called | denied
    actor_kind  TEXT,
    actor_id    TEXT,
    target_kind TEXT,
    target_id   TEXT,
    params      TEXT,            -- sanitized JSON (no secret values)
    run_id      TEXT,
    allowed     BOOLEAN NOT NULL,
    reason      TEXT
);
CREATE INDEX IF NOT EXISTS audit_log_ts ON audit_log(ts DESC);
CREATE INDEX IF NOT EXISTS audit_log_actor ON audit_log(actor_id, ts DESC);
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
| `bcrypt_cost` | int | `12` | bcrypt work factor for the stored passphrase hash; valid range 4–14 |
| `device_binding` | string | `off` | Bind trusted-device cookie to issuing IP subnet (/24, /48) + UA family. `off` \| `warn` \| `strict` |

Top-level security blocks in `Config` (siblings of `server:`, not nested under it):

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `audit_log.retention_days` | int | `30` | Days to retain `audit_log` rows; `0` disables pruning |
| `container_security.allow_host_network` | bool | `false` | Permit `network_mode: host`/`container:`/`ns:` |
| `container_security.allow_insecure_security_opt` | bool | `false` | Permit sandbox-disabling `security_opt` values |
| `container_security.allowed_cap_add` | []string | `[]` | Capabilities tasks may `cap_add` (`["ALL"]` = any) |
| `container_security.allowed_volume_roots` | []string | `[]` | Absolute host roots; non-empty = strict bind-mount allowlist |

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
| Stored passphrase is hashed | bcrypt at cost 12 (configurable 4–14), embedded cost lets verification keep working across cost changes |
| Lazy migration from legacy plaintext | On next successful login; race-free via `singleflight.Group` so concurrent logins yield one bcrypt + one write |
| 72-byte bcrypt limit enforced | API rejects passphrases > 72 bytes with `400` rather than silently truncating |
| Fail-closed on DB read errors | `passphraseSourceUnknown` → `503` from login + change endpoints; bootstrap fast-path never reached on transient outage |
| Password comparison is constant-time | `crypto/subtle.ConstantTimeCompare` (YAML), `bcrypt.CompareHashAndPassword` (DB hashes) |
| Webhook signatures are constant-time | `hmac.Equal` |
| CSRF protection | `SameSite=Strict` on all cookies |
| Clickjacking protection | `X-Frame-Options: SAMEORIGIN` |
| MIME sniffing protection | `X-Content-Type-Options: nosniff` |
| Device token rotation | Atomic DB transaction, old token deleted |
| IP spoofing guard | XFF only trusted with explicit `trust_proxy: true` |
| Structured audit log | Security-sensitive ops recorded at trust boundaries; secret values redacted (`pkg/audit`) |
| Container host-config floor | Dangerous docker/podman config rejected by default (`pkg/runtime/containersec`) |
| Brute force protection | 5 attempts/IP per minute (flat rate via go-chi/httprate) |
| Replay attack protection | 5-minute timestamp window on signed webhooks |
| CORS misconfiguration guard | Origins validated with `url.Parse()` at startup |
| Passphrase rotation requires current | bcrypt verify on `current` field |
| Per-task IPC socket in 0700 dir | On Linux/macOS each task run's Unix socket lives inside a per-run directory created `0700` (`/tmp/dicode-<runID>/ipc.sock`). The directory makes the socket unreachable to other local users independent of the socket file's own mode and the process umask. The socket file is also `chmod 0600` as belt-and-suspenders. |
| `dicode.lock` HMAC integrity | Approval records are HMAC-SHA256 signed with a key derived from the master key via Argon2id (`"dicode/approval-lock/v1"` context). v3 format: MAC covers `{bootstrapped, tasks}` so the bootstrap flag cannot be silently cleared. A MAC mismatch on load causes fail-closed: all records discarded, tasks require re-approval. Upgrade path: v1 unsigned and v2 tasks-only locks are accepted once and immediately upgraded to v3 in-place. |
| Daemon crypto namespace isolated | `permissions.dicode.crypto: ["*"]` never grants access to daemon-private sub-keys (e.g. `dicode/run-inputs/v1`); these are listed in `daemonPrivateCryptoContexts` in `pkg/ipc/server.go` and denied before any grant check |
| Replay retarget blocked | A task-scoped `dicode.runs.replay` call cannot redirect the replay at a different task ID — the target is pinned to the original run's task |
| `dicode` permission overrides are exhaustive | `mergeDicodePerms` merges all `DicodePermissions` fields including `secrets_has` and `crypto`; added exhaustiveness test guards against future fields being silently dropped |
| Pending-approval changes are reviewable, not blind | `Gate.Diff` (#604) surfaces a file-level diff of a pending task's changes — including a security-relevant highlight for permission/env/trigger edits — on both the dashboard's Approve button and the tokenized `/approve/{token}` link page before the operator confirms |
| Task list doesn't misrepresent a pending task as live (#650) | A held task's toggle no longer shows a plain "on" green dot, its trigger column no longer hyperlinks a webhook route that 404s until approved, and `Run` is disabled with a tooltip rather than silently 400ing behind a raw `alert()`. A pending count/filter and a notification-tray entry (wired to the existing `approval:pending` WebSocket event) make held tasks discoverable at a glance instead of requiring a per-row badge scan. |
