# Relay Client Design

## Problem

dicode instances running on developer laptops or behind corporate NAT/firewall
cannot receive incoming webhooks from external services (GitHub, Slack, Stripe,
etc.). The conventional solutions — ngrok, Cloudflare Tunnel, port forwarding —
require separate tooling, accounts, or network configuration that adds friction
and is not reproducible across machines.

The relay solves this by maintaining a persistent outbound WebSocket connection
from the local dicode daemon to a publicly reachable relay server. The relay
server receives inbound HTTP requests and forwards them over the WebSocket. The
local daemon reconstructs the HTTP request, runs it through the existing webhook
handler, and sends the HTTP response back over the same WebSocket.

The architecture requires no inbound ports, no NAT traversal, and no third-party
accounts for self-hosters.

---

## Architecture

```
GitHub / Slack / Stripe
        │
        │ POST /u/<uuid>/hooks/my-task
        ▼
┌──────────────────┐
│   Relay Server   │  (relay.dicode.app or self-hosted)
│                  │
│ /u/<uuid>/hooks/* ──── WebSocket ────────────────┐
└──────────────────┘                               │
                                                   │
                                        ┌──────────▼─────────┐
                                        │  dicode daemon     │
                                        │  (local machine)   │
                                        │                    │
                                        │  relay.Client      │
                                        │       │            │
                                        │  trigger.Engine    │
                                        │  WebhookHandler    │
                                        └────────────────────┘
```

The relay server proxies `POST /u/<uuid>/hooks/my-task` to the local daemon as
a WebSocket `request` message. The daemon makes a real HTTP request to its own
local HTTP server (`http://localhost:<port>/hooks/my-task`), captures the response,
and sends back a `response` message. The relay server translates that back into
an HTTP response to the original caller.

The client sets an `X-Relay-Base` header (`/u/<uuid>`) on every forwarded request
so the local server can generate correct relay-aware URLs (e.g. for `<base href>`
and SDK injection in webhook task UIs).

---

## Cryptographic Identity

### Key generation (first run)

1. Generate an ECDSA P-256 keypair using `crypto/rand`.
2. Store the PEM-encoded private key in the `kv` SQLite table (key:
   `relay.private_key`). The `kv` table already exists in the schema.
3. Derive the UUID: `hex(sha256(uncompressed P-256 public key bytes))` — 64
   lowercase hex characters. The uncompressed form is the 65-byte encoding
   `0x04 || X || Y`.

### Reconnection (stable identity)

On every subsequent start the daemon loads the private key from SQLite, derives
the same UUID, and presents the same stable public webhook URL. The URL never
changes as long as the database file is preserved.

### UUID derivation rationale

Using SHA-256 of the uncompressed public key as the identifier means:
- The relay server can verify `hex(sha256(presented_pubkey)) == claimed_uuid`
  without any server-side user database.
- The identifier is collision-resistant (SHA-256 preimage resistance).
- Clients cannot choose an arbitrary UUID; they must control the corresponding
  private key.

---

## Protocol Specification

All messages are JSON objects sent over a single WebSocket connection (text
frames). The connection is always initiated by the client (dicode daemon).

### Handshake

```
Server → Client  {"type":"challenge","nonce":"<64 hex chars>"}
Client → Server  {"type":"hello","uuid":"<64 hex>","pubkey":"<base64>","sig":"<base64>","timestamp":<unix>}
Server → Client  {"type":"welcome","url":"https://relay.example.com/u/<uuid>/hooks/"}
                 or
                 {"type":"error","message":"<reason>"}
```

**Challenge**: 32 random bytes encoded as 64 lowercase hex characters.

**Hello fields**:
- `uuid`: 64 hex chars derived as `hex(sha256(uncompressed_pubkey))`.
- `pubkey`: base64 (standard encoding) of the 65-byte uncompressed P-256 public
  key.
- `sig`: base64 ECDSA signature over `sha256(nonce_bytes || big-endian uint64
  timestamp)` using the private key. ASN.1 DER encoding.
- `timestamp`: Unix seconds at signing time.

**Verification steps** (server):
1. Decode `pubkey` from base64; reject if not 65 bytes starting with `0x04`.
2. Verify `hex(sha256(pubkey_bytes)) == uuid`; reject if mismatch.
3. Verify `timestamp` is within ±30 seconds of server clock; reject if outside.
4. Verify nonce has not been seen in the last 60 seconds; reject if replayed.
5. Verify ECDSA signature over `sha256(nonce_bytes || timestamp_be_uint64)`.

### Webhook forwarding

```
Server → Client  {
  "type":    "request",
  "id":      "<uuid>",
  "method":  "POST",
  "path":    "/hooks/my-task",
  "headers": {"X-Hub-Signature-256": ["sha256=..."]},
  "body":    "<base64>"
}

Client → Server  {
  "type":    "response",
  "id":      "<uuid>",
  "status":  200,
  "headers": {"Content-Type": ["application/json"]},
  "body":    "<base64>"
}
```

The `id` field correlates requests and responses. The relay server may
send multiple requests concurrently; the client handles each in a separate
goroutine and sends responses as they complete.

### Keepalive

Standard WebSocket ping/pong frames are used. The client sends a ping every
30 seconds. The relay server closes the connection if no pong is received
within 10 seconds of a ping.

---

## Threat Model

### Prevented

| Attack | Mitigation |
|---|---|
| UUID squatting | Server verifies `hex(sha256(pubkey)) == uuid`; you must hold the private key |
| Challenge replay | Timestamp must be within ±30 s; nonce is single-use (60 s TTL) |
| Signature forgery | ECDSA P-256; only the holder of the private key can sign |
| Connection hijacking | TLS (WSS) in production; MitM cannot read or inject frames |
| Enumeration of UUIDs | UUIDs are SHA-256 hashes; not guessable from the public URL |

### Not prevented

- **Relay server compromise**: The relay server sees plaintext request bodies and
  response bodies. A compromised relay server can read all webhook payloads and
  forge requests to the client. Mitigate by self-hosting the relay server on
  infrastructure you control.
- **Client key compromise**: An attacker who extracts the SQLite database gains
  the private key and can impersonate the client. Encrypt your disk or use the
  dicode secrets encryption layer.
- **Denial of service**: The relay server can drop connections or refuse to
  forward requests. The client reconnects automatically but cannot force the
  server to cooperate.
- **Traffic analysis**: An observer watching the WebSocket connection can infer
  that webhooks are being delivered (timing, sizes) even without reading content.

---

## Self-hosting vs Hosted Relay

| | Hosted (`relay.dicode.app`) | Self-hosted |
|---|---|---|
| Setup | Enable in config, done | Deploy relay server binary |
| Trust | Must trust dicode.app | You control the server |
| Availability | Managed, SLA | Your responsibility |
| Cost | May require paid plan | Free, infrastructure costs |
| Privacy | Relay sees plaintext payloads | You see your own payloads |

For high-security environments, self-host the relay server inside your network
perimeter. The Go relay server implementation (`pkg/relay/server.go`) can run
standalone. The production relay server is a separate Node.js service
(`dicode-relay` repo) that adds OAuth broker support and multi-client features.
