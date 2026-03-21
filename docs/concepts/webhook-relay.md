# Webhook Relay

Laptops and home machines are typically behind NAT and can't receive inbound HTTP connections. The webhook relay solves this: dicode connects to `relay.dicode.app` over a persistent WebSocket, and the relay forwards incoming webhook requests over that tunnel.

---

## How it works

```
GitHub / Stripe / any service
         ↓
   https://dicode.app/u/{uid}/hooks/{path}
         ↓ (HTTP)
   relay.dicode.app server
         ↓ (WebSocket tunnel, persistent)
   dicode process on your laptop
         ↓
   /hooks/{path} handler → task execution
```

1. On startup, dicode connects to `wss://relay.dicode.app` with your account token
2. The relay server assigns you a stable URL: `https://dicode.app/u/{uid}/hooks/{path}`
3. When a webhook arrives at that URL, the relay serializes it and sends it over the WebSocket
4. Your local dicode receives it, matches it to a webhook task, and executes it
5. The response is sent back over the WebSocket to the relay, which returns it to the caller

The tunnel is persistent — dicode reconnects automatically on disconnect with exponential backoff.

---

## Setup

1. Create a free account at [dicode.app](https://dicode.app)
2. Copy your relay token from the account settings
3. Add to `dicode.yaml`:

```yaml
relay:
  enabled: true
  token_env: DICODE_RELAY_TOKEN
```

4. Set the token:
```bash
dicode secrets set DICODE_RELAY_TOKEN your-token-here
```

5. Check the relay status:
```bash
dicode relay status
# Connected to relay.dicode.app
# Your webhook base URL: https://dicode.app/u/abc123/hooks/
```

---

## Webhook URLs

For a task with `trigger.webhook: /github-push`, the public URL is:

```
https://dicode.app/u/{uid}/hooks/github-push
```

Use this URL in GitHub, Stripe, or any webhook-capable service.

**Use in GitHub:**
1. Go to repo → Settings → Webhooks → Add webhook
2. Payload URL: `https://dicode.app/u/abc123/hooks/github-push`
3. Content type: `application/json`
4. Secret: your `server.webhook_secret` (optional but recommended)

---

## Relay status

```bash
dicode relay status
```

Shows:
- Connection status (connected / disconnected / disabled)
- Your UID and base webhook URL
- Requests forwarded (count, last 5)
- Relay server latency

---

## Self-hosted servers

If dicode is running on a VPS with a public IP, you don't need the relay — webhooks arrive directly at port 8080. Set `relay.enabled: false` (or omit the relay section).

The relay is designed for laptop/desktop users who want public webhook URLs without configuring port forwarding or maintaining a static IP.

---

## Plans

| Plan | Webhooks/month | Custom subdomain | Replay |
|---|---|---|---|
| Free | 500 | No | No |
| Pro ($12/mo) | Unlimited | Yes (`you.dicode.app`) | Yes (48h) |
| Team ($20/seat/mo) | Unlimited | Yes | Yes (7d) |

**Replay:** the relay stores the last N webhook payloads. If your local dicode was offline when a webhook arrived, you can replay it:
```bash
dicode relay replay --last 10
```

**Custom subdomain** (Pro+):
```
https://acme.dicode.app/hooks/github-push
```

Configure in `dicode.yaml`:
```yaml
relay:
  enabled: true
  token_env: DICODE_RELAY_TOKEN
  subdomain: acme   # Pro/Team only
```
