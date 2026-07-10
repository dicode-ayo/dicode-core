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
   Crypto: a **signing** keypair (mTLS client certificate, `/auth/:provider`
   request signatures) and a separate **decryption** keypair (ECIES recipient
   for OAuth token deliveries).
2. The private keys are exported as a `StoredIdentity` JSON blob (PKCS8,
   base64), encrypted via `dicode.crypto` under context
   `dicode/relay-identity/v1`, and persisted through the configured storage
   task under `<DATADIR>/relay-store/` (key `relay/identity-v1`). Nothing is
   written to the SQLite `kv` table and no PEM file exists.
3. Derive the UUID: `hex(sha256(uncompressed sign public key bytes))` — 64
   lowercase hex characters. The uncompressed form is the 65-byte encoding
   `0x04 || X || Y`.

The signing key doubles as the mTLS **client certificate** key. The task wraps
it in a self-signed X.509 leaf (CA:FALSE) presented on the WSS connection. The
broker derives the UUID from the peer certificate's public key, so the cert
carries the same identity as the signing key without a separate identifier.

### Reconnection (stable identity)

On every subsequent start the task fetches the encrypted blob, decrypts it via
`dicode.crypto`, derives the same UUID, and presents the same stable public
webhook URL. The URL never changes as long as the identity blob and the
secrets master key are preserved. If decryption fails (e.g. the master key was
rotated), the task regenerates a fresh identity — new UUID, new URLs.

### Broker authentication (server-cert verification)

The daemon authenticates the broker through normal TLS server-certificate
verification on the mTLS connection: the platform trust store (WebPKI) for the
hosted relay, or an explicit CA supplied via `relay.ca_file` for a self-hosted
broker with a self-signed cert. Trust derives from the authenticated TLS
channel, not from an application-level pin — there is no first-use pinning step
and no reconnect-time comparison.

The broker's OAuth delivery-signing public key still arrives in the `welcome`
frame and is persisted unconditionally, because the TLS channel already
authenticates the broker that sent it. Delivery envelopes remain
`broker_sig`-signed under that key.

### UUID derivation rationale

Using SHA-256 of the uncompressed public key as the identifier means:
- The broker can verify `hex(sha256(cert_pubkey)) == uuid` straight from the
  presented client certificate, without any server-side user database.
- The identifier is collision-resistant (SHA-256 preimage resistance).
- Clients cannot choose an arbitrary UUID; they must control the private key
  behind the client certificate.

---

## Protocol Specification

Messages are protojson-encoded `ServerMessage` / `ClientMessage` envelopes
(generated from `relay.proto`, dicode-relay#199) sent as JSON text frames over
a single WebSocket connection — a `oneof` wrapper keyed by variant name, e.g.
`{"hello":{...}}`, not a flat `{"type":...}` object. The connection is always
initiated by the client (dicode daemon). The pinned dicode-relay version
(`npm:dicode-relay@~0.2.0`, `deno.lock`) is the source of truth for the wire
format; `PROTOCOL_VERSION = 4` in `src/relay/server.ts`, and the client
(`src/client/handshake.ts`, `BROKER_PROTOCOL_MIN = 4`) rejects any broker
advertising a lower version.

The client encodes its own outbound messages with `useProtoFieldName: true`
(snake_case field names, e.g. `decrypt_pubkey`); the server's default
`toJson()` output uses camelCase (e.g. `brokerPubkey`). Field casing therefore
differs by direction — see the examples below.

### Transport: mutual TLS

The daemon connects to the broker's **dedicated mTLS listener**
(`server.mtls.port`, default 5554), separate from the public webhook/OAuth
listener (`server.port`, default 5553). The connection carries the identity in
the TLS layer:

- The daemon presents its self-signed X.509 **client certificate** (the signing
  key from [Key generation](#key-generation-first-run)). The TLS 1.3
  CertificateVerify message signs the handshake transcript with the cert's
  private key, channel-binding the key to this specific connection.
- The broker requires a client cert, derives `uuid = hex(sha256(cert_pubkey))`
  from the peer certificate, and admits the connection as that UUID. There is no
  application-level challenge, nonce, or signature exchange — TLS itself proves
  key ownership.
- The daemon verifies the broker's **server certificate** by normal TLS:
  WebPKI for the hosted relay, or the CA in `relay.ca_file` for a self-hosted
  broker with a self-signed cert.

Because identity is a property of the certificate key, the `/u/<uuid>` webhook
URL is unchanged from earlier protocol versions that derived the UUID from the
same signing key.

The mTLS listener requires **end-to-end TLS passthrough**. It must be exposed
directly or behind an L4/TCP load balancer — never behind a TLS-terminating
proxy, which strips the client certificate and causes every connection to be
rejected (close code 4401). The public listener (5553) may sit behind a
terminating proxy.

### Handshake

After the TLS handshake completes, the client opens with `hello` and the broker
replies `welcome`:

```
Client → Server  {"hello":{"uuid":"<64 hex>","pubkey":"<base64>","decrypt_pubkey":"<base64>"}}
Server → Client  {"welcome":{"url":"https://relay.example.com/u/<uuid>/hooks/","brokerPubkey":"<base64>","protocol":4}}
                 or
                 {"error":{"message":"<reason>"}}
```

**Hello fields**:
- `uuid`: 64 hex chars derived as `hex(sha256(uncompressed_sign_pubkey))`. The
  broker cross-checks it against the UUID derived from the client cert.
- `pubkey`: base64 (standard encoding) of the 65-byte uncompressed P-256
  **signing** public key. It matches the client-certificate key and is also
  used to verify `/auth/:provider` request signatures on the public listener.
- `decrypt_pubkey`: base64 (standard encoding) of the 65-byte uncompressed
  P-256 **decryption** public key — the ECIES recipient for OAuth token
  deliveries.

**Welcome fields**: `brokerPubkey` is the base64 SPKI DER key the broker uses
to sign delivery envelopes; the daemon persists it unconditionally, since the
TLS channel already authenticates the broker. `protocol` announces the protocol
version. Both sides reject `protocol < 4`.

**Broker admission checks**:
1. A client certificate is present (reject with close code 4401 otherwise — a
   TLS-terminating proxy in front of the listener triggers this).
2. The certificate key is P-256 (reject with 4402 otherwise).
3. `hex(sha256(cert_pubkey)) == hello.uuid` and the `hello.pubkey` matches the
   cert key; reject the `hello` (close code 4400) on mismatch.

Close codes on the mTLS listener: **4400** bad hello, **4401** no client
certificate (or one stripped by a proxy), **4402** certificate key not P-256.

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
| UUID squatting | Broker derives the UUID from the client cert key and verifies `hex(sha256(cert_pubkey)) == uuid`; you must hold the private key |
| Signature/handshake relay (relay #98) | TLS 1.3 CertificateVerify channel-binds the key to the connection transcript; a captured handshake cannot be replayed onto another connection, so a MitM cannot claim another daemon's UUID |
| Rogue broker | The daemon verifies the broker's server cert (WebPKI or `relay.ca_file`); a substituted endpoint fails TLS and cannot receive OAuth deliveries |
| Connection hijacking | Mutual TLS 1.3; a MitM cannot read or inject frames, nor present a valid client cert |
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

`tasks/buildin/relay-server` runs this same service in-process under dicode's
own Deno runtime — the quickest way to self-host. Its status endpoint
password defaults to a documented dev value (`dicode-relay-dev`) when
`RELAY_STATUS_PASSWORD` isn't set in the local secrets store. The task refuses
to start with that default in effect on a non-loopback `base_url`, and always
warns loudly when it's active at all — see
[`docs/concepts/task-format.md`](../concepts/task-format.md) for the pipeline
shape and set a real password with
`dicode secrets set RELAY_STATUS_PASSWORD <password>` before exposing it.
