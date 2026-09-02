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
  public_url: ""                      # scheme://host[:port] that notification links are built from; requires auth
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

The session-cookie check and the device-token renewal/write-back both live in a
single helper, `hasValidSession` — `requireAuth` just calls it. Every other
session-consuming gate (`webhookAuthGuard` for `trigger.auth: session`/`any`
webhooks, and `sessionOrAPIKeyMiddleware` behind
`requireSessionOrNonEphemeralAPIKey` — e.g. `/api/tasks/{id}/approve`,
`/api/runs/{id}/replay`, `/api/runs/{id}/resume`; `/mcp` is Bearer API-key
only, see #698) calls the same
helper, so a rotated device cookie is written back to the response and the
scs session is marked `authenticated` identically no matter which route the
request came in through (#681).

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

### server.public_url — the address links are built from

Every link dicode puts in an outbound notification comes from `WebUIBaseURL()`
([authoring_service.go](../../pkg/webui/authoring_service.go)): the `resume_url`
on the suspend hook and the `approve_url` on the approval hook. Without
`public_url` these read `localhost:<port>` — `https` when `tls_cert` and
`tls_key` are both set, `http` otherwise — which resolves to the recipient's
own machine the moment the notification leaves the host, so the operator learns
something is waiting and cannot act on it.

`public_url` records the address the daemon already answers on, whether that is
a reverse proxy, a tailnet name or a LAN host. It does not make the daemon
reachable; that stays the operator's problem.

**It is refused unless `server.auth` is set.** Publishing an off-loopback
address is only safe behind the auth wall. With auth off, `requireAuth`
early-returns and every "authenticated" endpoint falls open —
`POST /api/tasks/{id}/approve` among them — so a reachable address hands the
whole control plane, and the approval gate with it, to any unauthenticated
caller. A fronting proxy that authenticates does not lift the requirement: the
daemon cannot verify that it does, so `trust_proxy` (which only governs
`X-Forwarded-For` parsing) is not accepted as a substitute.
Run such a proxy *and* `server.auth: true` — the daemon's own wall is cheap
insurance against the proxy being misconfigured or bypassed on the LAN.

**Only a bare authority is accepted.** The web UI serves root-relative URLs, so
mounting it under a subpath would fix the notification link and break every
page behind it — a path, query or fragment is refused rather than carried.
Credentials are refused for a blunter reason: this address is pasted verbatim
into every notification, Telegram's servers included. An empty trailing `/`,
`?` or `#` carries nothing and is dropped, since callers append
`/approve/<token>` straight onto the stored value.

**`http://` is accepted.** The `/approve/{token}` link is a bearer credential in
a URL, so a plaintext hop exposes it to anyone on the path. It is still allowed:
the address is usually a tailnet or VPN name where the transport is encrypted
below HTTP, and dicode requires TLS nowhere else. Prefer `https` — either
`tls_cert`/`tls_key` here or termination at the proxy — whenever the hop is not
already encrypted.

**The two links are not equally usable remotely.** `/approve/{token}` is
session-less by design — the single-use token in the path is the credential —
so it works from a phone. `resume_url` lands on the dashboard and still needs a
login at the far end.

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
    commit: 4f2b9c8e...   # 40-hex; absent when no commit describes the task
```

`commit:` is the git commit the approved content was observed at, captured when
the task is held pending rather than when the operator clicks approve, so it
describes the version that was reviewed rather than wherever the repository has
moved to by the time the operator clicks.

It is recorded only when HEAD actually tracks the task's directory. A source
outside any repository records none, and so does one whose tasks merely sit
inside an unrelated repository — a folder under a version-controlled home
directory — since that repository's HEAD describes none of their content.

The commit is decoration: the review surface uses it to show what moved since
the last approval, and no gate decision reads it. A task with no recorded commit
approves and arms exactly like one that has it.

Records written before `commit:` existed keep verifying: the field is
`omitempty`, so an absent commit contributes nothing to the MAC payload and
those records still hash to the bytes they were signed over.

The `bootstrapped: true` field is omitted (YAML `omitempty`) until
`MarkBootstrapped()` is called. Legacy unsigned files (`version: 1`, no `mac:`)
and v2 signed files are accepted on the first startup with a key and immediately
upgraded to v3. After that, any subsequent load that sees a missing or wrong MAC
treats the file as tampered.

---

## Pending-Change Review

`dicode.lock` stores a content hash, never file bytes, so by the time a task
shows up pending the working tree already holds the *new* content and there is
no "before" on disk. Rather than reconstruct one, the review surface renders
**end state**: the resolved task as it will run if the operator arms it.

The reframe is that under the trust model above the operator has already seen
this change on the git host, with line comments, blame and CI status — by the
time dicode pends the task, the diff has been reviewed and merged. The decision
in front of the operator is not *what moved*, it is **what will run if I arm
this**. See `docs/design/approval-review-surface.md` and ADR-0001..0003.

### What the surface shows

`Gate.State` (`pkg/approval/state.go`) builds an `approval.State` from the
pending task's parsed spec:

- **Runs as** — runtime, container image, network mode, resolved timeout.
- **Fires on** — every resolved trigger, with the concrete cron expression or
  webhook path, its auth mode, and whether a webhook secret is configured.
- **Can reach** — the resolved, post-override permission set: `net`, `fs`,
  `run`, `sys`, the `dicode` API grants, and unrestricted-env-read.
- **Environment** — one declaration per entry: the name the task sees and where
  the value comes from (`host` / `secret` / `task` / `literal`).
- **Params** — name, type, required, and whether a default is configured.
- **Container** — volumes, ports, added capabilities, user, and the *names* of
  literal container env vars.
- **Stages** — for `kind: PipelineTask`, the stage list and which stages patch
  their target's spec.
- **Files** — a per-file inventory: path, size, and a SHA-256 over exactly the
  bytes the content hash folds in for that file (`task.Inventory`). This is the
  one code-shaped fact the spec cannot carry, so a new or edited file is visible
  without any content being rendered.

Because it derives from the checkout rather than a baseline, a task with no git
history, no prior approval and no cached snapshot still gets a complete surface.
A dirty working tree is not a special case: the daemon runs the checkout, so
what is shown is what will run.

### Two invariants

**No secret is dereferenced at render time.** An env entry declaring
`from: env:GH_TOKEN` renders as `API_KEY ← host env GH_TOKEN`; the reference is
never followed. Literal values written inline in `task.yaml` — the `- KEY=VALUE`
shorthand, a `params[].default`, `docker.env_vars`, chain-edge params — never
render at all, only their presence or their names. Effective permissions still
resolve (policy, not values) and taskset overrides still resolve (config, not
values); only the value lookup stops.

`pkg/secrets/redactor.go` does not help here and must not be reached for. It is
value-based — it string-replaces the values resolved for a *run* in that run's
log output — so it cannot mask a literal that was never in the secrets store,
and it is built per run while a pending task has not run. See ADR-0003.

**No code bytes render in dicode.** The git host renders code better, and the
operator has already been there. No blob is opened, so there is no size cap, no
truncation banner and no "too large to display" state to design.

### Surfaces

- `GET /api/tasks/{id}/pending-state` — same auth group as
  `POST /api/tasks/{id}/approve` (`requireSessionOrNonEphemeralAPIKey`). 404
  unknown task, 409 not pending, 503 gate not wired.
- **Task detail** (`dc-task-detail.js`) — the panel opens by default for a
  pending task. Approve is armed only while a successfully fetched panel is on
  screen: collapsing it, a failed fetch, or a still-loading fetch all disarm,
  because each would be approving without having seen what will run. The task
  list has no approve button at all — a table row has nowhere to show a review,
  so it hands off to the detail page.
- `/approve/{token}` — the session-less link travels through Slack, email or
  ntfy, so whatever it renders is visible to everyone in that channel. It shows
  the task ID and its short hash, and nothing about the task's contents.

**Approval binds to the reviewed hash.** The panel sends
`State.PendingHash` back with the approve request (#645). Between the panel
loading and the click landing, a push can re-pend the task at a newer hash; the
server rejects the stale approval with `{"stale": true}` and the dashboard
re-fetches to the version actually pending rather than silently arming content
the operator never reviewed.

**Out of scope:** `dicode task approve` (the CLI) does not render end state; it
goes over the control-socket IPC and is the deliberate escape hatch for when the
dashboard cannot render a review at all.

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
| `public_url` | string | `""` | Address the daemon is reachable at from outside the machine; notification links are built from it. `scheme://host[:port]` — no path, query, fragment or credentials. Rejected unless `auth` is set |
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
| Pending-approval changes are reviewable, not blind | `Gate.State` renders the resolved task a pending change would arm — triggers, effective permissions, env declarations and a per-file inventory — on the dashboard before the operator confirms, without rendering code or dereferencing a secret |
| Per-run IPC capability tokens require a real signing key | `ipc.IssueToken`/`ipc.VerifyToken` (`pkg/ipc/token.go`) fail closed with an error when handed a nil/empty secret, rather than signing/verifying under an implicit all-zero HMAC key. Both runtimes' per-version executors (`runtimes.deno.version` / `runtimes.python.version` pinned) snapshot the daemon's real `IPCSecret` at construction (`pkg/runtime/deno/manager.go`, `pkg/runtime/python/runtime.go`), and `pkg/daemon/runtimes_test.go` asserts it's non-nil for both — see [#718](https://github.com/dicode-ayo/dicode-core/issues/718). |
| Task list doesn't misrepresent a pending task as live (#650) | A held task's toggle no longer shows a plain "on" green dot, its trigger column no longer hyperlinks a webhook route that 404s until approved, and `Run` is disabled with a tooltip rather than silently 400ing behind a raw `alert()`. A pending count/filter and a notification-tray entry (wired to the existing `approval:pending` WebSocket event) make held tasks discoverable at a glance instead of requiring a per-row badge scan. |
| A resolved sub-tree can't exfiltrate env credentials via git auth, or smuggle a task ref in as a TaskSet (#740) | `Resolver.resolveRef` (`pkg/taskset/resolver.go`) only honours a git ref's `auth.token_env` for refs declared in operator-owned config — `dicode.yaml`'s source ref, or an entry in a source's root `taskset.yaml` (`allowAuth=true`). A ref discovered while recursing into an already-resolved `TaskSet` entry (`resolveNestedRef`, always `allowAuth=false`) has its `token_env` stripped and the drop logged, so a writable source cannot smuggle in a nested taskset naming an arbitrary daemon env var as an HTTP basic-auth credential. Independently, a ref that resolves to a `task.yaml`/`task.yml` file — the exact shape `taskset.AddTaskEntry` writes for every scaffolded task, and any directory-valued ref whose probe lands on one — is refused as a load failure if the file at that path declares `kind: TaskSet`, so a task-ref-shaped entry can never be routed into taskset recursion (with its ref-auth and entry-merge machinery) purely by what the file's own `kind:` field claims. |
| Operator allowlist for which env var a trusted git ref may name as `token_env` (#753) | `source_security.allowed_token_envs` in `dicode.yaml` narrows *which variable* an already-trusted ref (`allowAuth=true`, see #740 above) may name as `auth.token_env` — without it, anyone with push access to a git source's root `taskset.yaml` can still name any daemon env var (e.g. an unrelated provider API key) as a credential to hand to a host they control. `Resolver.ensureClone` (`pkg/taskset/resolver.go`) checks the token against the allowlist after `gatedTokenEnv`, stripping and logging it if unlisted. The zero value (unset) is fully permissive, matching pre-#753 behavior. |
