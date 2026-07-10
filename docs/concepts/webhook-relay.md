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
  server_url: wss://relay.dicode.app   # or ws://localhost:5553 for local dev
```

When `relay.enabled` is `true` and `server_url` is set, the daemon dispatches the `buildin/relay-client` daemon task on boot. The task generates a stable cryptographic identity on first run (see below) and reconnects automatically with exponential backoff on disconnect. An optional `broker_url` field overrides the OAuth broker base URL; when empty it is derived from `server_url` by swapping the scheme.

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
1. **Relay handshake** -- the daemon proves ownership of the UUID via ECDSA challenge-response (sign key)
2. **OAuth broker** -- the daemon signs auth requests so the broker can verify the caller controls the relay UUID (sign key)
3. **Token encryption** -- the broker encrypts OAuth tokens to the daemon's decryption public key (ECIES)

The task also pins the broker's public key on first connect (trust-on-first-use); a later mismatch aborts the connection. The pin is stored encrypted alongside the identity.

The UUID stays stable as long as the identity blob and the secrets master key both survive. If the master key changes (passphrase rotation), the blob is unrecoverable and the task regenerates a fresh identity with a new UUID.

---

## Protocol

All messages are JSON text WebSocket frames.

### Handshake

```
Server -> Client   {"type":"challenge","nonce":"<64 hex chars>"}
Client -> Server   {"type":"hello","uuid":"...","pubkey":"...","decrypt_pubkey":"...","sig":"...","timestamp":N}
Server -> Client   {"type":"welcome","url":"https://relay.dicode.app/u/<uuid>/hooks/","broker_pubkey":"...","protocol":2}
                   or
                   {"type":"error","message":"<reason>"}
```

`pubkey` is the signing public key; `decrypt_pubkey` is the separate key the broker encrypts OAuth token deliveries to. `broker_pubkey` on the welcome is what the client TOFU-pins.

Verification steps (server):
1. Decode `pubkey` from base64 -- must be 65 bytes starting with `0x04`
2. Verify `hex(sha256(pubkey_bytes)) == uuid`
3. Verify `timestamp` within +/-30 s of server clock
4. Verify nonce not seen in last 60 s
5. Verify ECDSA signature over `sha256(nonce_bytes || timestamp_big_endian_uint64)`

### Request forwarding

```
Server -> Client   {"type":"request","id":"<uuid>","method":"POST","path":"/hooks/my-task","headers":{...},"body":"<base64>"}
Client -> Server   {"type":"response","id":"<uuid>","status":200,"headers":{...},"body":"<base64>"}
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
- Same protocol, same handshake verification
- In-memory nonce store, client registry
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
