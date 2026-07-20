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
  server_url: wss://relay.dicode.app:5554   # broker mTLS endpoint
  broker_url: https://relay.dicode.app      # public OAuth/webhook base URL
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
  enabled: true             # default: false
  server_url: wss://...     # broker mTLS endpoint (single-instance shorthand)
  broker_url: https://...   # public OAuth/webhook base URL
  ca_file: /path/ca.pem     # optional: CA for a self-signed broker
```

For a high-availability relay deployment with more than one broker instance,
list every instance's mTLS endpoint under `server_urls` instead of `server_url`:

```yaml
relay:
  enabled: true
  server_urls:              # one mTLS endpoint per broker instance
    - wss://relay-a.example.com:5554
    - wss://relay-b.example.com:5554
  broker_url: https://relay.example.com   # shared public OAuth/webhook base URL
```

The daemon holds one independent mTLS connection per entry, all sharing its
identity and client certificate, so every instance registers the same daemon
uuid and any instance can forward an inbound webhook locally — no directory, no
mesh, instant failover.

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Enable the relay client |
| `server_url` | string | — | WebSocket URL of the broker's mTLS listener (e.g. `wss://relay.example:5554`). Must start with `wss://`; a `ws://` URL is rejected at config load because mTLS requires TLS. Single-instance shorthand — mutually exclusive with `server_urls` |
| `server_urls` | string list | — | One mTLS listener URL per broker instance for an HA deployment. Every entry must be `wss://`; duplicate or empty entries are rejected at config load. Mutually exclusive with `server_url`; when the relay is enabled exactly one of the two must be set. The OAuth broker origin is derived from the first entry when `broker_url` is unset |
| `broker_url` | string | derived | Public base URL for OAuth and webhook delivery (e.g. `https://relay.example`). The mTLS and public listeners run on different ports, so set this explicitly. When empty it is derived from the (first) control-channel URL, and the daemon warns at boot if that URL carries an explicit non-default port. Must be `http://` or `https://` when set |
| `ca_file` | string | — | PEM CA bundle used to verify the broker's server certificate. Set it for a self-hosted broker with a self-signed cert; leave empty for the hosted relay, which is verified against the platform trust store (WebPKI) |

**Every `server_urls` entry must be an mTLS control endpoint reachable with
end-to-end TLS passthrough (L4 only)** — never behind a terminating proxy
(Cloudflare orange-cloud, nginx `listen ... ssl`, etc.), which strips the
client certificate and the broker closes the connection with code 4401. Only
the separate public webhook/OAuth listener (`broker_url`) may sit behind such a
proxy. A multi-instance broker deployment must also run with
`server.multi_instance: true` and a **shared broker signing key and mTLS
certificate** across all instances: the daemon persists the broker public key
per connection (last write wins), so OAuth `broker_sig` verification is only
consistent when every instance presents the identical key.

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

The 64-hex identifier is `sha256(sign_public_key)`. The broker derives it from
the daemon's mTLS client certificate, which wraps that same signing key, so the
URL is stable across the mTLS transport. It is collision-resistant and cannot
be guessed or squatted by another party.

---

## Backing up and rotating the relay identity

The relay identity is an encrypted blob at
`<DATADIR>/relay-store/identity-v1.bin`. It is encrypted at rest via
`dicode.crypto`, whose sub-keys derive from the secrets master key — a copied
blob is only restorable alongside the same master key.

**Backup:** copy the blob together with the master key material — the
`<DATADIR>/master.key` file (auto-generated, chmod 600), or the
`DICODE_MASTER_KEY` env value if you provision the key that way. The secrets
database is not required to restore the relay identity. Restoring the blob
on a machine with a different master key fails to decrypt, and the relay
client regenerates a fresh identity (new UUID, new webhook URLs).

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

The broker exposes **two listeners**:

- **mTLS listener** (`server.mtls.port`, default 5554) — where daemons
  connect. It authenticates each daemon by its client certificate, so it
  requires **end-to-end TLS passthrough**. Expose it directly or behind an
  L4/TCP load balancer only; **never** behind a TLS-terminating proxy
  (Cloudflare orange-cloud, nginx `listen ... ssl`, etc.). Termination strips
  the client certificate and every connection is rejected with WS close code
  4401.
- **Public listener** (`server.port`, default 5553) — inbound webhooks and the
  OAuth callback flow. This one *may* sit behind a TLS-terminating reverse
  proxy (nginx, Caddy, Cloudflare).

Configure clients to point `server_url` at the mTLS listener and `broker_url`
at the public base URL:

```yaml
relay:
  enabled: true
  server_url: wss://relay.example.com:5554
  broker_url: https://relay.example.com
  ca_file: /etc/dicode/relay-ca.pem
```

The relay server requires no database; the client registry is kept in memory.

Alternatively, the `buildin/relay-server` pipeline task runs dicode-relay
in-process under the daemon's Deno runtime — useful for standing up a
local or single-tenant relay without a separate deployment. On first run it
auto-generates a self-signed mTLS server certificate at
`${DICODE_DATADIR}/relay/mtls-cert.pem` (CA:FALSE, SANs covering `localhost`
and the `BASE_URL` host). Point each connecting daemon's `relay.ca_file` at
that PEM so it trusts the broker; the hosted relay needs no `ca_file` because
its certificate chains to a public CA.

> The daemon repository no longer carries a Go relay implementation; the former
> `pkg/relay/server.go` and `pkg/relay/oauth.go` are gone in favour of the
> TypeScript relay — see [`docs/design/oauth-broker.md`](design/oauth-broker.md)
> for rationale.
