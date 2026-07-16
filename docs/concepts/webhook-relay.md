# Webhook Relay

The webhook relay lets a local dicode instance receive incoming HTTP requests (webhooks, OAuth callbacks, asset serving) from external services without port forwarding, ngrok, or a public IP address.

---

## How it works

The `buildin/relay-client` daemon task (built on `npm:dicode-relay/client`) maintains a persistent outbound WebSocket connection to a relay server. The relay server receives inbound HTTP requests at a stable public URL and forwards them over the WebSocket. The task reconstructs the HTTP request, forwards it to the local HTTP server, captures the response, and sends it back.

```
GitHub / Slack / Stripe
        |
        | POST /u/<uuid>/hooks/my-task
        v
+------------------+
|   Relay Server   |  (relay.dicode.app or self-hosted)
|                  |
| /u/<uuid>/*  -------- WebSocket ----------+
+------------------+                        |
                                            |
                                 +----------v-----------+
                                 |  dicode daemon       |
                                 |  (local machine)     |
                                 |                      |
                                 |  buildin/relay-client|
                                 |  task (npm:          |
                                 |  dicode-relay/client)|
                                 |       |              |
                                 |  local HTTP server   |
                                 |  (trigger engine,    |
                                 |   webhook handler,   |
                                 |   dicode.js)         |
                                 +----------------------+
```

No inbound ports, no NAT traversal, no third-party accounts required.

---

## Configuration

```yaml
relay:
  enabled: true
  server_url: wss://relay.dicode.app:5554   # broker mTLS endpoint
  broker_url: https://relay.dicode.app      # public OAuth/webhook base URL
```

When `relay.enabled` is `true` and `server_url` is set, the daemon dispatches the `buildin/relay-client` daemon task on boot. The task generates a stable cryptographic identity on first run (see below) and reconnects automatically with exponential backoff on disconnect. `server_url` must be `wss://` — it points at the broker's dedicated mTLS listener. `broker_url` is the public base URL for OAuth and webhook delivery; because the mTLS and public listeners run on different ports, set it explicitly. An optional `ca_file` supplies a PEM CA for a self-signed broker.

---

## Stable public URL

After a successful handshake the relay server returns a webhook base URL:

```
https://relay.dicode.app/u/<uuid>/hooks/
```

The `<uuid>` is derived from the daemon's ECDSA P-256 signing public key (`hex(sha256(uncompressed_sign_pubkey))`). It never changes as long as the encrypted identity blob and the secrets master key are preserved. Use this URL as the webhook endpoint in GitHub, Slack, Stripe, etc.

The relay also serves `/u/<uuid>/dicode.js` so that webhook task UIs work through the relay with no extra configuration.

---

## Cryptographic identity

On first run the `buildin/relay-client` task generates a split P-256 identity: a **signing** keypair and a separate **decryption** keypair. The private keys are exported as a `StoredIdentity` JSON blob, encrypted via `dicode.crypto` (context `dicode/relay-identity/v1`, sub-keys derived from the secrets master key), and persisted via the configured storage task under `<DATADIR>/relay-store/`. There is no PEM file and no SQLite `kv` row. The UUID is derived from the signing key:

```
UUID = hex(sha256(0x04 || X || Y))    // 64 lowercase hex characters, sign pubkey
```

This identity is used for:
1. **Relay handshake** -- the daemon presents a self-signed X.509 client certificate wrapping the sign key; the broker derives the UUID from the cert and mutual TLS proves key ownership (sign key)
2. **OAuth broker** -- the daemon signs auth requests so the broker can verify the caller controls the relay UUID (sign key)
3. **Token encryption** -- the broker encrypts OAuth tokens to the daemon's decryption public key (ECIES)

The daemon authenticates the broker by verifying its TLS server certificate — the platform trust store (WebPKI) for the hosted relay, or the CA in `relay.ca_file` for a self-signed broker. There is no first-use key pin; trust comes from the authenticated TLS channel.

The UUID stays stable as long as the identity blob and the secrets master key both survive. If the master key changes (passphrase rotation), the blob is unrecoverable and the task regenerates a fresh identity with a new UUID.

---

## Protocol

The daemon connects to the broker's dedicated mTLS listener (default port 5554),
separate from the public webhook/OAuth listener (default 5553). It presents a
self-signed client certificate; the broker derives the UUID from the cert key,
and TLS 1.3 proves ownership without any application-level challenge or
signature. The mTLS listener requires end-to-end TLS passthrough — a
TLS-terminating proxy in front of it strips the client cert and every
connection is rejected (close code 4401).

Messages are protojson-encoded `ServerMessage` / `ClientMessage` envelopes
(generated from `relay.proto`) sent as JSON text WebSocket frames — a `oneof`
wrapper keyed by variant name (e.g. `{"hello":{...}}`), not a flat
`{"type":...}` object. The wire format is defined by the pinned dicode-relay
package (`npm:dicode-relay@~0.2.0`, see `deno.lock`); see
[docs/design/relay.md](../design/relay.md) for the full protocol reference.
Both client and broker require protocol version 4 — a broker advertising a
lower version is rejected.

### Handshake

After the TLS handshake, over the established mTLS connection:

```
Client -> Server   {"hello":{"uuid":"...","pubkey":"...","decrypt_pubkey":"..."}}
Server -> Client   {"welcome":{"url":"https://relay.dicode.app/u/<uuid>/hooks/","brokerPubkey":"...","protocol":4}}
                   or
                   {"error":{"message":"<reason>"}}
```

`pubkey` is the signing public key (matching the client-certificate key); `decrypt_pubkey` is the separate key the broker encrypts OAuth token deliveries to. `brokerPubkey` on the welcome is the broker's delivery-signing key, which the daemon persists unconditionally. (The client encodes `hello` with proto field names/snake_case; the server's `welcome`/`error` use camelCase — casing differs by direction.)

Broker admission checks:
1. A client certificate is present, with a P-256 key
2. Decode `pubkey` from base64 -- must be 65 bytes starting with `0x04`, matching the cert key
3. Verify `hex(sha256(cert_pubkey)) == uuid`

### Request forwarding

```
Server -> Client   {"request":{"id":"<uuid>","method":"POST","path":"/hooks/my-task","headers":{...},"body":"<base64>"}}
Client -> Server   {"response":{"id":"<uuid>","status":200,"headers":{...},"body":"<base64>"}}
```

The client handles multiple concurrent requests and sends responses as they complete.

---

## Client security boundaries

The relay client enforces these restrictions before forwarding requests to the local daemon:

| Rule | Reason |
|------|--------|
| Path must start with `/hooks/` or be `/dicode.js` | Limits blast radius if relay server is compromised |
| `X-Relay-Base` header from server is stripped and replaced | Prevents relay server from spoofing the relay base path |
| Hop-by-hop headers (`Connection`, `Transfer-Encoding`, etc.) stripped from responses | HTTP/1.1 compliance |
| `Set-Cookie` stripped from responses | Prevents cookie injection to external callers |
| Body limited to 5 MB | Prevents memory exhaustion |
| Local HTTP timeout: 25 s | Prevents hung connections |

---

## Relay-aware SDK injection

When a webhook task includes an `index.html`, the trigger engine injects the dicode SDK (`<script src="/dicode.js">`). If the request arrives through the relay, the `X-Relay-Base` header (set by the relay client) adjusts the `<base href>` and script paths so that the UI works correctly at the relay URL.

---

## Relay server options

### Hosted (`relay.dicode.app`)

The production relay server is a separate TypeScript/Node.js service (`dicode-relay` repo). It adds:
- OAuth broker (Grant + Express) for zero-setup OAuth with 14 providers
- ECIES token encryption (P-256 ECDH + HKDF + AES-256-GCM)
- Status dashboard with real-time metrics
- Multi-client support

### Self-hosted

The relay server lives at [dicode-ayo/dicode-relay](https://github.com/dicode-ayo/dicode-relay) — a Node.js service that can run standalone for environments where you control the infrastructure:
- Same protocol, same mTLS admission
- Dedicated mTLS listener (default 5554) requiring TLS passthrough, plus the public listener (default 5553); daemons trust its self-signed server cert via `relay.ca_file`
- Optional OAuth broker (configurable via `relay.yaml`)
- Published as a Docker image; suitable for embedding in tests and single-user self-hosting

| | Hosted | Self-hosted |
|---|---|---|
| Setup | Enable in config | Deploy relay server binary |
| Trust | Must trust dicode.app | You control the server |
| OAuth | Zero-setup, 14 providers | Not included |
| Cost | May require paid plan | Free, your infrastructure |

---

## Reconnection

The npm `RelayClient` reconnects the WSS connection automatically with internal exponential backoff. The `buildin/relay-client` task wraps everything outside the WSS lifecycle (missing config, storage or decrypt failures) in an outer 5 s → 60 s exponential backoff loop. The daemon's UUID and webhook URLs remain the same across reconnects.

---

## OAuth over the relay

Encrypted OAuth token deliveries arrive on the reserved `/hooks/oauth-complete` path, handled by `buildin/auth-relay`. See [oauth.md](../oauth.md) for the full flow.
