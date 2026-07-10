# Relay

The dicode relay lets a local dicode instance receive webhooks from GitHub,
Slack, Stripe, and other external services without port forwarding, ngrok, or
any inbound firewall rules. The daemon makes a single outbound WebSocket
connection to a relay server; the relay server forwards incoming HTTP requests
over that connection, and the daemon sends back HTTP responses. Your local
webhook tasks work exactly as they do in production — the relay is transparent.

---

## Quick start

**1. Enable relay in `dicode.yaml`:**

```yaml
relay:
  enabled: true
  server_url: wss://relay.dicode.app
```

**2. Start the daemon:**

```
dicode daemon --config dicode.yaml
```

On first start the `buildin/relay-client` task generates a split P-256
sign + decrypt identity, stores it encrypted at rest under
`<DATADIR>/relay-store/`, and logs the public webhook URL:

```
{"level":"info","msg":"relay connected","url":"https://relay.dicode.app/u/4a7b3c.../hooks/"}
```

**3. Copy the URL to your webhook provider:**

In GitHub → Settings → Webhooks, set the Payload URL to:

```
https://relay.dicode.app/u/<your-uuid>/hooks/github-push
```

That's it. The URL is stable across restarts as long as you keep your data
directory (the identity blob) and your secrets master key.

---

## Config reference

```yaml
relay:
  enabled: true          # default: false
  server_url: wss://...  # relay WebSocket endpoint
  broker_url: https://.. # optional: OAuth broker base URL override
```

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Enable the relay client |
| `server_url` | string | — | WebSocket URL of the relay server (must start with `wss://` in production) |
| `broker_url` | string | derived | OAuth broker base URL. When empty, derived from `server_url` by swapping the scheme (`wss://host` → `https://host`). Set it when the broker runs on a different host, or during local development to point at a non-TLS port (e.g. `http://localhost:5553`). Must be `http://` or `https://` when set. |

---

## Stable URL

The webhook URL is derived from a cryptographic keypair, not from your IP
address or hostname. As long as the identity blob and the secrets master key
are intact:

- Restarts do not change your URL.
- Changing networks does not change your URL.
- The relay reconnects automatically after network interruptions.

The URL has the form:

```
https://<relay-host>/u/<64-hex-chars>/hooks/
```

The 64-hex identifier is `sha256(sign_public_key)`. It is stable,
collision-resistant, and cannot be guessed or squatted by another party.

---

## Backing up and rotating the relay identity

The relay identity is an encrypted blob at
`<DATADIR>/relay-store/identity-v1.bin`. It is encrypted at rest via
`dicode.crypto`, whose sub-keys derive from the secrets master key — a copied
blob is only restorable alongside the same master key.

**Backup:** copy the blob together with the secrets database (which holds the
master key material). Restoring the blob on a machine with a different master
key fails to decrypt, and the relay client regenerates a fresh identity (new
UUID, new webhook URLs).

**Rotation** (e.g. suspected key compromise): stop the daemon, delete
`<DATADIR>/relay-store/identity-v1.bin`, restart. The relay-client task
generates a fresh identity on next boot. All previously shared webhook URLs
stop working; reissue them as needed.

Keep the blob and your master key secret. Anyone holding both can impersonate
your relay client.

---

## OAuth broker

When connected to a relay that exposes the OAuth broker (the default at
`relay.dicode.app`), the daemon gains two built-in tasks that perform the
authorization flow without any provider-side app registration:

- `buildin/auth-start` — signs a `/auth/:provider` URL with the daemon's
  relay identity and prints it for the user to open.
- `buildin/auth-relay` — receives the encrypted token delivery on the
  reserved `/hooks/oauth-complete` path, verifies the broker signature,
  ECIES-decrypts the envelope, and writes each credential to secrets via
  `dicode.secrets_set`. It runs locked down (`silent: true`, no network,
  no filesystem, minimal env) so decrypted tokens cannot leak through
  logs or side channels.

The full flow, security model, and failure modes are documented in
[oauth.md](./oauth.md).

If the relay is disabled (or no broker URL can be derived),
`buildin/auth-start` fails with "relay broker URL not configured
(DICODE_RELAY_BROKER_URL)" — use the local OAuth flow instead (see the
same doc).

---

## Self-hosting the relay server

The relay server lives in a separate repository:
[dicode-ayo/dicode-relay](https://github.com/dicode-ayo/dicode-relay) — a
single-process TypeScript/Node.js 22 service that combines the WebSocket
tunnel and the OAuth broker. A ready-to-run Docker image is published via
the repo's release pipeline (see the relay repo's `Dockerfile`).

Configure clients to point at your self-hosted instance:

```yaml
relay:
  enabled: true
  server_url: wss://relay.example.com
```

The relay server requires no database; nonce state and client registry are
kept in memory. TLS termination should be handled by a reverse proxy
(nginx, Caddy, etc.) in front of the Node process.

Alternatively, the `buildin/relay-server` pipeline task runs dicode-relay
in-process under the daemon's Deno runtime — useful for standing up a
local or single-tenant relay without a separate deployment.

> Historically the daemon repository carried a Go relay implementation at
> `pkg/relay/server.go`. It was dropped in favour of the TypeScript relay —
> see [`docs/design/oauth-broker.md`](design/oauth-broker.md) for rationale.
