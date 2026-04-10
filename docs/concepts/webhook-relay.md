# Webhook Relay

The webhook relay lets a local dicode instance receive incoming HTTP requests (webhooks, OAuth callbacks, asset serving) from external services without port forwarding, ngrok, or a public IP address.

---

## How it works

The dicode daemon maintains a persistent outbound WebSocket connection to a relay server. The relay server receives inbound HTTP requests at a stable public URL and forwards them over the WebSocket. The daemon reconstructs the HTTP request, forwards it to the local HTTP server, captures the response, and sends it back.

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
                                 |  relay.Client        |
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

When `relay.enabled` is `true` and `server_url` is set, the daemon starts the relay client on boot. The relay client generates a stable cryptographic identity on first run (see below) and reconnects automatically with exponential backoff on disconnect.

---

## Stable public URL

After a successful handshake the relay server returns a webhook base URL:

```
https://relay.dicode.app/u/<uuid>/hooks/
```

The `<uuid>` is derived from the daemon's ECDSA P-256 public key (`hex(sha256(uncompressed_pubkey))`). It never changes as long as the daemon's database file is preserved. Use this URL as the webhook endpoint in GitHub, Slack, Stripe, etc.

The relay also serves `/u/<uuid>/dicode.js` so that webhook task UIs work through the relay with no extra configuration.

---

## Cryptographic identity

On first run the daemon generates an ECDSA P-256 keypair and stores the PEM-encoded private key in the SQLite `kv` table (key: `relay.private_key`). The UUID is derived as:

```
UUID = hex(sha256(0x04 || X || Y))    // 64 lowercase hex characters
```

This identity is used for:
1. **Relay handshake** -- the daemon proves ownership of the UUID via ECDSA challenge-response
2. **OAuth broker** -- the daemon signs auth requests so the broker can verify the caller controls the relay UUID
3. **Token encryption** -- the broker encrypts OAuth tokens to the daemon's public key (ECIES)

---

## Protocol

All messages are JSON text WebSocket frames.

### Handshake

```
Server -> Client   {"type":"challenge","nonce":"<64 hex chars>"}
Client -> Server   {"type":"hello","uuid":"...","pubkey":"...","sig":"...","timestamp":N}
Server -> Client   {"type":"welcome","url":"https://relay.dicode.app/u/<uuid>/hooks/"}
                   or
                   {"type":"error","message":"<reason>"}
```

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

The client handles multiple concurrent requests. Each is dispatched in a separate goroutine.

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

The Go relay server (`pkg/relay/server.go`) can run standalone for environments where you control the infrastructure:
- Same protocol, same handshake verification
- In-memory nonce store, client registry
- No OAuth broker -- just plain webhook forwarding
- Designed for embedding in tests and single-user self-hosting

| | Hosted | Self-hosted |
|---|---|---|
| Setup | Enable in config | Deploy relay server binary |
| Trust | Must trust dicode.app | You control the server |
| OAuth | Zero-setup, 14 providers | Not included |
| Cost | May require paid plan | Free, your infrastructure |

---

## Reconnection

The client reconnects automatically with exponential backoff (1 s initial, 60 s max, +/-20% jitter). The backoff resets after 10 s of stable connection. The daemon's UUID and webhook URLs remain the same across reconnects.

---

## What is not yet built

- **OAuth token delivery**: The relay client does not yet handle ECIES-encrypted token payloads from the broker. Design is in `docs/design/oauth-broker.md`.
- **`config.relay.broker_url`**: Config field for the OAuth broker URL (for `tasks/auth/_oauth-app/task.ts` broker mode).
