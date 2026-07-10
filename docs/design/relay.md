# Relay Client Design

## Problem

dicode instances running on developer laptops or behind corporate NAT/firewall
cannot receive incoming webhooks from external services (GitHub, Slack, Stripe,
etc.). The conventional solutions — ngrok, Cloudflare Tunnel, port forwarding —
require separate tooling, accounts, or network configuration that adds friction
and is not reproducible across machines.

The relay solves this by maintaining a persistent outbound WebSocket connection
from the local dicode daemon (via the `buildin/relay-client` task, built on
`npm:dicode-relay/client`) to a publicly reachable relay server. The relay
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
                                        │  buildin/          │
                                        │  relay-client task │
                                        │  (npm:dicode-relay │
                                        │   /client)         │
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

1. The `buildin/relay-client` task generates a split P-256 identity via Web
   Crypto: a **signing** keypair (WSS handshake, `/auth/:provider` request
   signatures) and a separate **decryption** keypair (ECIES recipient for
   OAuth token deliveries).
2. The private keys are exported as a `StoredIdentity` JSON blob (PKCS8,
   base64), encrypted via `dicode.crypto` under context
   `dicode/relay-identity/v1`, and persisted through the configured storage
   task under `<DATADIR>/relay-store/` (key `relay/identity-v1`). Nothing is
   written to the SQLite `kv` table and no PEM file exists.
3. Derive the UUID: `hex(sha256(uncompressed sign public key bytes))` — 64
   lowercase hex characters. The uncompressed form is the 65-byte encoding
   `0x04 || X || Y`.

### Reconnection (stable identity)

On every subsequent start the task fetches the encrypted blob, decrypts it via
`dicode.crypto`, derives the same UUID, and presents the same stable public
webhook URL. The URL never changes as long as the identity blob and the
secrets master key are preserved. If decryption fails (e.g. the master key was
rotated), the task regenerates a fresh identity — new UUID, new URLs.

### Broker pin (trust-on-first-use)

On the first successful handshake the client stores the broker's public key
(from the `welcome` message) encrypted under context
`dicode/relay-broker-pin/v1` (key `relay/broker-pin-v1`). Subsequent connects
compare the presented broker key against the pin and abort on mismatch, so a
relay endpoint swapped out from under the operator cannot silently take over
OAuth deliveries.

### UUID derivation rationale

Using SHA-256 of the uncompressed public key as the identifier means:
- The relay server can verify `hex(sha256(presented_pubkey)) == claimed_uuid`
  without any server-side user database.
- The identifier is collision-resistant (SHA-256 preimage resistance).
- Clients cannot choose an arbitrary UUID; they must control the corresponding
  private key.

---

## Protocol Specification

Messages are protojson-encoded `ServerMessage` / `ClientMessage` envelopes
(generated from `relay.proto`, dicode-relay#199) sent as JSON text frames over
a single WebSocket connection — a `oneof` wrapper keyed by variant name, e.g.
`{"hello":{...}}`, not a flat `{"type":...}` object. The connection is always
initiated by the client (dicode daemon). The pinned dicode-relay version
(`npm:dicode-relay@~0.1.4`, `deno.lock`) is the source of truth for the wire
format; `PROTOCOL_VERSION = 3` in `src/relay/server.ts`, and the client
(`src/client/handshake.ts`, `BROKER_PROTOCOL_MIN = 3`) rejects any broker
advertising a lower version.

The client encodes its own outbound messages with `useProtoFieldName: true`
(snake_case field names, e.g. `decrypt_pubkey`); the server's default
`toJson()` output uses camelCase (e.g. `brokerPubkey`). Field casing therefore
differs by direction — see the examples below.

### Handshake

```
Server → Client  {"challenge":{"nonce":"<64 hex chars>"}}
Client → Server  {"hello":{"uuid":"<64 hex>","pubkey":"<base64>","decrypt_pubkey":"<base64>","sig":"<base64>","timestamp":<unix>}}
Server → Client  {"welcome":{"url":"https://relay.example.com/u/<uuid>/hooks/","brokerPubkey":"<base64>","protocol":3}}
                 or
                 {"error":{"message":"<reason>"}}
```

**Challenge**: 32 random bytes encoded as 64 lowercase hex characters.

**Hello fields**:
- `uuid`: 64 hex chars derived as `hex(sha256(uncompressed_sign_pubkey))`.
- `pubkey`: base64 (standard encoding) of the 65-byte uncompressed P-256
  **signing** public key, used for ECDSA verification (handshake and
  `/auth/:provider` signatures).
- `decrypt_pubkey`: base64 (standard encoding) of the 65-byte uncompressed
  P-256 **decryption** public key — the ECIES recipient for OAuth token
  deliveries.
- `sig`: base64 ECDSA signature over `sha256(nonce_bytes || big-endian uint64
  timestamp)` using the signing private key. ASN.1 DER encoding.
- `timestamp`: Unix seconds at signing time.

**Welcome fields**: `brokerPubkey` is the base64 SPKI DER key the broker uses
to sign delivery envelopes (the client TOFU-pins it); `protocol` announces the
protocol version. Both sides reject `protocol < 3` — v3 means "types
generated from proto".

**Verification steps** (server):
1. Decode `pubkey` from base64; reject if not 65 bytes starting with `0x04`.
2. Verify `hex(sha256(pubkey_bytes)) == uuid`; reject if mismatch.
3. Verify `timestamp` is within ±30 seconds of server clock; reject if outside.
4. Verify nonce has not been seen in the last 60 seconds; reject if replayed.
5. Verify ECDSA signature over `sha256(nonce_bytes || timestamp_be_uint64)`.

### Webhook forwarding

```
Server → Client  {"request": {
  "id":      "<uuid>",
  "method":  "POST",
  "path":    "/hooks/my-task",
  "headers": {"X-Hub-Signature-256": {"values": ["sha256=..."]}},
  "body":    "<base64>"
}}

Client → Server  {"response": {
  "id":      "<uuid>",
  "status":  200,
  "headers": {"Content-Type": {"values": ["application/json"]}},
  "body":    "<base64>"
}}
```

`headers` is a proto3 map whose value type (`HeaderValues`) wraps a repeated
string as `{"values": [...]}`, since proto3 maps can't hold repeated values
directly.

The `id` field correlates requests and responses. The relay server may
send multiple requests concurrently; the client handles them concurrently
(npm `ws` client) and sends responses as they complete.

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
- **Client key compromise**: The identity blob is encrypted at rest via
  `dicode.crypto`, so an attacker needs both the blob under
  `<DATADIR>/relay-store/` and the secrets master key to impersonate the
  client. An attacker with full access to the data directory and key material
  can do so; disk encryption still helps.
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
perimeter. The relay server lives in a separate repo
([`dicode-ayo/dicode-relay`](https://github.com/dicode-ayo/dicode-relay)) —
a Node.js service with WebSocket tunnel + OAuth broker support. An earlier
Go implementation at `pkg/relay/server.go` was removed in favour of the
TypeScript service; see [`docs/design/oauth-broker.md`](oauth-broker.md) for
the rationale.
