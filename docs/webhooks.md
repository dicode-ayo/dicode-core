# Webhooks

dicode exposes an HTTP gateway that routes incoming requests to webhook tasks.
No separate relay process or tunnel is needed — tasks register their URL path
and the daemon dispatches requests directly.

---

## Defining a webhook task

```yaml
# tasks/github-push/task.yaml
apiVersion: dicode/v1
kind: Task
name: github-push
description: Handle GitHub push events
runtime: deno
trigger:
  webhook: /hooks/github-push
permissions:
  env:
    - SLACK_TOKEN
timeout: 30s
```

When the daemon loads this task the path `/hooks/github-push` is automatically
registered in the HTTP gateway. Requests to that path spawn the task and return
its result as the HTTP response.

---

## Authenticating webhooks

### HMAC signature (recommended for external services)

Use `webhook_secret` to verify the `X-Hub-Signature-256` header automatically.
The task script only runs if the signature is valid; a missing or wrong
signature returns HTTP 403.

```yaml
trigger:
  webhook: /hooks/github-push
  webhook_secret: "${GITHUB_WEBHOOK_SECRET}"
permissions:
  env:
    - GITHUB_WEBHOOK_SECRET
```

Always reference a secret, never hardcode the value.

### Session auth (internal tools)

Require a logged-in dicode session for both `GET` (serving the task UI) and
`POST` (running the task):

```yaml
trigger:
  webhook: /hooks/my-internal-tool
  auth: true
```

Public webhooks (no `auth: true`) remain fully open.

`auth` is tri-valued:

- `true` / `"session"` — session required. With a `webhook_secret` also set, session **and** signature are both required.
- `"any"` — session **or** a valid HMAC signature (requires `webhook_secret`). Lets a signed machine caller authenticate over the public relay URL, where session cookies never travel; a browser still authenticates directly via its session. `GET`/asset requests always require a session, never a signature.
- absent / `false` — public.

```yaml
trigger:
  webhook: /hooks/my-machine-endpoint
  auth: any
  webhook_secret: "${MY_WEBHOOK_SECRET}"
```

### GET requests not supported with webhook_secret

When `webhook_secret` is configured, only POST requests are accepted.
GET requests are rejected with HTTP 403. This is intentional: GET requests
carry no body, so `HMAC(secret, "")` is a constant value that does not bind to
any request-specific data — it cannot authenticate a GET, and using one would
allow (a) replay DoS (all GETs share the same HMAC digest so the second request
is always rejected as a duplicate) and (b) signature reuse across different
query strings.

Open webhooks (no `webhook_secret`) continue to accept GET requests for
backward compatibility.

### Timestamp-bound HMAC

When the sender includes the `X-Dicode-Timestamp` header (Unix seconds as a
decimal string), dicode validates that the timestamp is within a 5-minute
tolerance window and then **includes the timestamp in the HMAC preimage**:

```
HMAC-SHA256(secret, "<timestamp_unix_str>\n<body>")
```

Without a timestamp the preimage is just the body (backwards-compatible):

```
HMAC-SHA256(secret, "<body>")
```

Including the timestamp in the signed payload binds the signature to a specific
time window. This means a captured request cannot be replayed even after the
1-hour replay cache expires, because the timestamp value changes with every
legitimately signed request.

Because sending `X-Dicode-Timestamp` is optional, a sender that never sends it
(intentionally or not) stays replayable indefinitely once the request leaves
the 1-hour cache window or the daemon restarts — a downgrade attack isn't
possible (stripping the header breaks the signature), but a permanently
timestamp-less signer is. Set `require_timestamp: true` to close that gap by
rejecting any request that omits the header — recommended for any
relay-exposed webhook, where requests already leave your network boundary:

```yaml
trigger:
  webhook: /hooks/my-task
  webhook_secret: "${SECRET}"
  require_timestamp: true
```

`require_timestamp` defaults to `false` so senders that can't be made to emit
a custom header — GitHub's webhook delivery, for example — keep working
unmodified with the plain `HMAC-SHA256(secret, body)` preimage above.

Example — signing a request with a timestamp in Python:

```python
import hmac, hashlib, time

ts = str(int(time.time()))
preimage = (ts + "\n").encode() + body
sig = "sha256=" + hmac.new(secret.encode(), preimage, hashlib.sha256).hexdigest()

headers = {
    "X-Dicode-Timestamp": ts,
    "X-Hub-Signature-256": sig,
}
```

### Replay protection

When `webhook_secret` is set, dicode automatically rejects duplicate webhook
requests within a 1-hour window. This prevents an attacker who captures a
valid request from replaying it — the task fires once and subsequent
duplicates return HTTP 409.

The replay cache is keyed on the exact same preimage the signature covers:
`HMAC(secret, "<timestamp>\n<body>")` when the request carries
`X-Dicode-Timestamp`, or `HMAC(secret, body)` when it doesn't. Binding the
timestamp into the key (rather than hashing the body alone) means two
legitimately distinct requests with an identical body — a `{}` heartbeat sent
twice a minute apart, say — don't collide as a false-positive replay.

This key change doesn't, by itself, bound how long a captured request stays
replayable after a daemon restart — the cache is in-memory and empty after
any restart regardless of key shape. What bounds that window is the
timestamp check itself (independent of the cache): with a timestamp present,
a captured request older than 5 minutes is rejected outright, restart or
not. `require_timestamp` (above) is what extends that bound to senders who'd
otherwise send no timestamp at all.

Replay protection is enabled by default. Opt out per task if your sender
legitimately sends byte-identical payloads:

```yaml
trigger:
  webhook: /hooks/idempotent-task
  webhook_secret: "${SECRET}"
  replay_protection: false
```

Open webhooks (no `webhook_secret`) are unaffected.

### Concurrency cap applies to webhook-triggered runs

When `max_concurrent_tasks` is configured in `dicode.yaml`, the cap applies to
webhook-triggered runs as well as cron and manual runs. When all slots are
occupied, the webhook handler returns HTTP 503 (Service Unavailable) immediately
rather than queuing the request indefinitely. The caller should retry after a
short delay.

### Duplicate webhook paths are rejected

Each webhook path can only be claimed by one task. If a second task (e.g., a
task loaded from a watched git repo) tries to register a path that is already
claimed, it is silently rejected and a warning is logged:

```
WARN rejecting duplicate webhook path — already claimed by another task
     path=/hooks/my-path existing_task=original-task rejected_task=intruder-task
```

A task re-registering its own path during a reconciler reload is allowed.

---

## Accessing request data in the task

The parsed POST body (JSON or form fields) is available via the `input` global:

```typescript
// task.ts
const action = input.action       // GitHub push event field
const repo   = input.repository   // nested objects fully available
```

Query-string parameters are available via `params`.

---

## Exposing webhooks to the internet

dicode listens on the configured port (default `8080`). For local development
or self-hosted instances you need to make this port reachable from the internet
to receive webhooks from services like GitHub or Stripe.

Recommended options:

| Tool | Command |
|---|---|
| [ngrok](https://ngrok.com) | `ngrok http 8080` |
| [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) | `cloudflared tunnel --url http://localhost:8080` |
| [Tailscale Funnel](https://tailscale.com/kb/1223/funnel) | `tailscale funnel 8080` |
| Reverse proxy (nginx, Caddy) | Proxy `yourdomain.com` → `localhost:8080` |

Point the external service's webhook URL at:

```
https://your-public-host/hooks/<path>
```

---

## Daemon tasks that serve HTTP (http.register)

A daemon task can register arbitrary HTTP patterns at startup via the
`http.register` IPC method. This is used by daemon tasks that run a persistent
HTTP server (e.g., a custom UI or API proxy):

```typescript
// task.ts — daemon task with mcp_port serving a custom UI
// task.yaml: trigger: { daemon: true }

// Register a catch-all handler for this task's namespace.
await dicode.http.register("/my-app/*")

// The task SDK delivers each incoming request as an event:
dicode.http.serve(async (req) => {
  if (req.path === "/my-app/") {
    return { status: 200, headers: { "Content-Type": "text/html" }, body: "<h1>My App</h1>" }
  }
  return { status: 404, body: "not found" }
})
```

Registered patterns are automatically unregistered when the daemon task exits.
Pattern priority: exact match wins; for equal-length patterns, first-registered
wins. Built-in daemon routes (`/health`, `/api/*`) always have priority.
