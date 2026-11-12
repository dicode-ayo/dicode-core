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

### Signing with the `dicode webhook sign` CLI

Rather than hand-rolling the HMAC above, `dicode webhook sign` computes it for
you and prints the exact header lines to send. It's pure local crypto — no
running daemon required — so it works in a shell script, a CI job, or another
service that needs to call a protected dicode webhook.

```text
dicode webhook sign --secret <secret> [flags]

Flags:
  --secret <value>       HMAC secret (required)
  --data <string>        inline request body
  --data-file <path>     read the request body from a file
                          (default: read the body from stdin if neither
                          --data nor --data-file is given)
  --timestamp <unix-sec> explicit Unix timestamp to sign
                          (default: current time; mutually exclusive with
                          --no-timestamp)
  --no-timestamp         omit the timestamp and sign the bare body
                          (GitHub-compatible mode; mutually exclusive with
                          --timestamp)
```

Output goes to stdout as the two header lines (the timestamp line is omitted
with `--no-timestamp`):

```text
X-Hub-Signature-256: sha256=<hex>
X-Dicode-Timestamp: <unix_ts>
```

Example — sign a payload and curl it straight at a protected webhook:

```bash
BODY='{"hello":"world"}'
HEADERS=$(dicode webhook sign --secret "$MY_WEBHOOK_SECRET" --data "$BODY")

curl -X POST https://your-host/hooks/my-task \
  -H "Content-Type: application/json" \
  -H "$(echo "$HEADERS" | grep '^X-Hub-Signature-256:')" \
  -H "$(echo "$HEADERS" | grep '^X-Dicode-Timestamp:')" \
  -d "$BODY"
```

Use `--no-timestamp` when you need the plain `HMAC-SHA256(secret, body)`
preimage — the same GitHub-compatible mode described above.

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

Webhook triggers and daemon-task `http.register` routes share one route table,
and each path can only be claimed by one task. A claim of a path another task
already holds — a second webhook task loaded from a watched git repo, or a
daemon task calling `http.register` on another task's path — is rejected and
the original task keeps serving it. The one exception: a webhook declared in
`task.yaml` takes its path over from another task's `http.register` route (a
`webhook trigger took over another task's http.register route` warning is
logged), so a daemon cannot squat a path before the task that declares it is
armed. The daemon is not given the path back when that task is later removed;
it has to call `http.register` again. A task held for approval holds no route
at all: its path answers 404, and its UI and assets are not served, until it is
approved. For a rejected webhook trigger a warning is logged (once; repeats on
later retries are logged at debug level):

```
WARN rejecting webhook path  task=intruder-task path=/hooks/my-path
     error="route already claimed by another task: /hooks/my-path is held by original-task"
```

and an `http.register` call returns the same error to the task. A task
re-claiming its own path (a reconciler reload, a daemon restart) is allowed.
`/hooks/oauth-complete` is reserved for `buildin/auth-relay`: no other task may
claim it, no path that would prefix-match it (such as `/hooks`) can be claimed,
and no shorter route ever answers for it. A trailing slash is ignored:
`/hooks/x/` and `/hooks/x` are the same path.

### Route matching

A request is served by the task whose path equals the request path, otherwise
by the longest claimed path that is a whole-segment prefix of it — with both
`/hooks/ai` and `/hooks/ai/openai` claimed, `/hooks/ai/openai/chat.js` goes to
the second and `/hooks/ai/other.js` to the first; `/hooks/aix` matches neither.
The auth guard decides on the same match the request is then dispatched to, so
a public `/hooks/ai` can never expose a protected `/hooks/ai/openai`.

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

A daemon task (`trigger.daemon: true`) can claim an HTTP path with the
`http.register` IPC method and answer the requests routed to it. There is no
SDK wrapper yet: the task opens its own connection to `DICODE_SOCKET`,
handshakes with `DICODE_TOKEN`, and exchanges length-prefixed JSON frames
(see `tests/e2e/fixtures/tasks/http-register-guarded/task.js`):

- `{ "id": "1", "method": "http.register", "pattern": "/hooks/my-app" }` claims
  the path and everything below it. The pattern must be below `/hooks/`, and
  may not sit above or below another task's route (a daemon cannot claim
  `/hooks/webui/runs` under the web UI's `/hooks/webui`); the reply carries an
  `error` if it is invalid, reserved, held by another task, or nested with one.
  A task that later declares the same path as its `trigger.webhook` takes it over.
- Each request arrives as a push `{ requestID, httpMethod, path, query,
  reqHeaders, reqBody }` (`reqBody` base64), answered with
  `{ "method": "http.respond", requestID, status, respHeaders, respBody }`
  within 60 s.

Registering a new pattern releases the previous one. The route is released
when the daemon's IPC connection closes — including when the run is stopped
because the task was removed, changed, or held for approval.

**Auth:** an `http.register` route inherits the daemon task's `trigger.auth`.
With `auth: true` (or `"session"`) every request below the path needs a dicode
session — unauthenticated browsers are redirected to `/login`, API callers get
401 — exactly as for a webhook task. Nothing on this route verifies an HMAC
signature, so `auth: "any"` admits sessions only. A route whose owning task is
no longer in the registry answers 404.

```yaml
trigger:
  daemon: true
  auth: true
```
