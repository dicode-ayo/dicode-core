# Webhooks

dicode exposes an HTTP gateway that routes incoming requests to webhook tasks.
No separate relay process or tunnel is needed — tasks register their URL path
and the daemon dispatches requests directly.

---

## Defining a webhook task

```yaml
# tasks/github-push/task.yaml
name: github-push
description: Handle GitHub push events
runtime: deno
trigger:
  webhook: /hooks/github-push
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
