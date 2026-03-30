# Security Plan — dicode

> **Status**: Phases 1–4 implemented, code-reviewed, and hardened (PR #11 + review fixes).
> Phase 5 (multi-user RBAC) is the north star — design documented, not yet built.
>
> For deep implementation details see [Security Developer Reference](./concepts/security.md).

---

## Threat Model

| Threat | Pre-auth risk | Current state |
| --- | --- | --- |
| Unauthenticated task execution via `/api/tasks/{id}/run` | HIGH — fully open | Blocked by global auth wall |
| Unauthenticated webhook trigger via `/hooks/*` | HIGH — path-only protection | Blocked by HMAC signature verification |
| Config exfiltration/modification via `/api/config/raw` | HIGH — open | Blocked by global auth wall |
| MCP lateral movement (run_task, switch_dev_mode) | HIGH — open JSON-RPC | Blocked by API key middleware |
| Secrets theft via `/api/secrets` | MEDIUM — passphrase-gated | Preserved + extended |
| CSRF | LOW — SameSite=Strict on secrets cookie | Extended to all session and device cookies |
| Session hijacking after restart | MEDIUM — in-memory tokens lost | SQLite-backed device tokens survive restarts |
| Webhook replay attacks | MEDIUM — no timestamp check | Mitigated by HMAC + X-Dicode-Timestamp (5 min window) |
| Timing attacks on password compare | LOW — subtle.ConstantTimeCompare | Preserved |
| CORS overreach | MEDIUM — wildcard `*` | Tightened to explicit `server.allowed_origins` allowlist |
| Missing security headers | LOW | CSP, Permissions-Policy, existing headers all present |

### Trust boundary

dicode is self-hosted. The expected deployment:

```text
internet ──TLS──▶ reverse proxy (nginx/Caddy) ──▶ dicode :8080 (localhost)
```

TLS is terminated at the proxy. dicode handles its own auth. The plan also supports direct exposure on a trusted LAN.

---

## Phase 1 — Global Auth Wall ✅

**Config**:

```yaml
server:
  auth: true
  secret: "your-passphrase"
  allowed_origins: []   # empty = same-origin only
  trust_proxy: false    # set true when behind nginx/Caddy
```

**What was built**:

- `requireAuth` middleware in `pkg/webui/auth.go` — gates all routes when `server.auth: true`; API requests get 401, browser navigations get a redirect to `/?auth=required`
- Always-public paths: `POST /api/secrets/unlock`, `POST /api/auth/refresh`, `/app/*` static assets, `/sw.js`, `/hooks/*` (webhook auth is HMAC-based, not session-based)
- `corsMiddleware` replaces the former wildcard with an explicit `server.allowed_origins` list; unrecognised origins receive no `Access-Control-Allow-Origin` header; entries are validated with `url.Parse()` at startup — malformed entries are skipped and logged rather than silently corrupting the allowlist
- `securityHeaders` extended with `Content-Security-Policy` and `Permissions-Policy`
- `X-Forwarded-For` only trusted when `server.trust_proxy: true` — prevents direct clients from spoofing their IP to bypass the login rate limiter
- Login rate limiter extended: on the 5th failed attempt the lockout window extends to **15 minutes** (not just 1 minute)

---

## Phase 2 — Trusted Browser ✅

**How it works**:

1. User POSTs `{"password":"…","trust":true}` to `/api/secrets/unlock`
2. Server issues a short-lived **session cookie** (8 h) and a long-lived **device cookie** (30 d)
3. On subsequent visits: session expired → SPA silently POSTs to `/api/auth/refresh` with the device cookie → new session issued, no login prompt
4. User sees all trusted devices in the **Security** page and can revoke any of them individually or all at once

**Storage**: SQLite `sessions` table — token stored as SHA-256 hash only; raw token lives only in the cookie.

**Session token design**: session tokens are generated from `crypto/rand` (32 bytes). The passphrase plays no role in token generation or validation — tokens are validated by in-memory map lookup only. This means knowing the passphrase does not allow forging a session token.

**Device token rotation**: `renewFromDevice()` is wrapped in a `db.Tx()` transaction. When a device token's age exceeds `deviceRotateAfter` (24h), the old row is deleted and a new token is inserted atomically. The new raw token is returned to the caller so the browser's device cookie is refreshed. This prevents long-lived tokens from accumulating without ever cycling.

**New endpoints**:

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/secrets/unlock` | Login; `trust:true` issues device cookie |
| `POST` | `/api/auth/refresh` | Silent session renewal from device cookie |
| `GET` | `/api/auth/devices` | List trusted devices |
| `DELETE` | `/api/auth/devices/{id}` | Revoke one device |
| `POST` | `/api/auth/logout` | Revoke current session + device |
| `POST` | `/api/auth/logout-all` | Emergency: wipe all sessions and devices |

**UI**: `dc-auth-overlay` modal injected by `app.js` — intercepts every 401 from `api.js`, attempts silent refresh, then shows the passphrase form with "Trust this browser for 30 days" checkbox. The original request retries transparently after login.

---

## Phase 3 — Webhook HMAC Authentication ✅

**Task spec**:

```yaml
trigger:
  webhook: /hooks/my-task
  webhook_secret: "${MY_WEBHOOK_SECRET}"
```

**Behaviour**:

- When `webhook_secret` is set: every incoming request must carry `X-Hub-Signature-256: sha256=<hmac>` computed over the raw request body using the secret. Requests without a valid signature are rejected with HTTP 403 before the task script runs.
- When `webhook_secret` is absent: the webhook is open (backwards-compatible).
- **Replay protection**: if the sender includes `X-Dicode-Timestamp: <unix>`, requests older than 5 minutes are rejected.
- **GitHub-compatible**: the signature format matches GitHub's webhook delivery exactly — point a GitHub webhook at a dicode endpoint with the same secret and it works with no client changes.

**Implementation**: `verifyWebhookSignature` in `pkg/trigger/engine.go`; body capped at 5 MB before HMAC computation.

**Body capture**: the raw request body is read into a `[]byte` slice **before** any content-type parsing. For `application/x-www-form-urlencoded` requests the bytes are replayed back via `bytes.NewReader` so `r.ParseForm()` can still work. This ensures HMAC is always computed over the actual request bytes regardless of content-type.

**Example**: `examples/github-push-webhook/` — full working task that receives GitHub push events, verifies the signature, and renders a commit summary.

---

## Phase 4 — MCP API Key Authentication ✅

**Key format**: `dck_<64 hex chars>` — greppable prefix, long enough to be unguessable.

**Storage**: SHA-256 hash stored in `api_keys` SQLite table; raw key shown once at creation.

**Usage**:

```http
Authorization: Bearer dck_A3kR9pQz…
```

**Endpoints** (all require a valid session):

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/auth/keys` | List keys (no raw values) |
| `POST` | `/api/auth/keys` | Create key; returns raw key once |
| `DELETE` | `/api/auth/keys/{id}` | Revoke a key |

**UI**: API key management on the `/security` page — create, copy (one-time), revoke.

---

## Phase 5 — Multi-User RBAC (North Star)

Not yet implemented. Design:

### Roles

| Role | Capabilities |
| --- | --- |
| `admin` | Everything: manage users, config, secrets, run tasks |
| `operator` | Run tasks, view runs/logs, manage own devices/keys |
| `viewer` | Read-only: list tasks, view runs and logs |

### Proposed schema additions

```sql
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,   -- argon2id
    role          TEXT NOT NULL DEFAULT 'operator',
    created_at    INTEGER NOT NULL,
    last_login    INTEGER
);

-- sessions and api_keys gain a user_id FK
ALTER TABLE sessions  ADD COLUMN user_id TEXT REFERENCES users(id);
ALTER TABLE api_keys  ADD COLUMN user_id TEXT REFERENCES users(id);
```

### Bootstrap

When `server.auth: true` and no users exist in the DB, dicode creates a single `admin` user from `server.secret`. Subsequent runs use the `users` table and ignore `server.secret`. This is a seamless one-time upgrade.

### Password hashing

`argon2id` via `golang.org/x/crypto/argon2` (parameters: m=64 MB, t=3, p=4) — current OWASP recommendation.

---

## WebUI Standalone Consideration

> Referenced from dicode-core PR #9: the WebUI may become a standalone deployable task.

If the WebUI moves to a separate origin:

- Session cookies won't work cross-origin → switch to short-lived JWTs stored in `sessionStorage` with `Authorization: Bearer` header
- `server.allowed_origins` must include the WebUI origin explicitly
- Device trust token: works if both share a parent domain via `Domain=` cookie attribute; otherwise use a server-side refresh token flow
- `server.serve_ui: false` flag to disable the embedded static SPA

The `/api/auth/*` endpoints are already designed to work with header-based auth (no cookie required) to support this transition.

---

## Deployment Checklist

Before exposing dicode outside localhost:

- [ ] `server.auth: true` in `dicode.yaml`
- [ ] Strong passphrase in `server.secret` (≥ 20 chars, high entropy — longer is better since the passphrase is hashed for comparison only, not used to derive tokens)
- [ ] TLS terminated at the reverse proxy (nginx / Caddy)
- [ ] `server.trust_proxy: true` **only** if sitting behind a proxy (otherwise IP rate limiting can be bypassed)
- [ ] `server.allowed_origins` set to the exact WebUI origin (if served from a separate origin)
- [ ] Webhook tasks using `webhook_secret:` for any public-facing endpoint
- [ ] MCP API key generated from `/security` page and set in Claude / agent config
- [ ] dicode port not directly reachable from the internet (only via proxy)
- [ ] `dicode.yaml` not world-readable (`chmod 600`)
