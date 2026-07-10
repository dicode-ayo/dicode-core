# buildin/relay-client

Daemon task that maintains the WebSocket tunnel to the dicode relay
broker. Identity (split P-256 sign + decrypt keys) is generated on first
boot, persisted encrypted-at-rest via `dicode.crypto` + the configured
storage task, and reused on subsequent restarts.

## Required env (set by daemon at boot)

- `DICODE_RELAY_SERVER_URL` — `wss://relay.example/` (from `relay.server_url` in dicode.yaml)
- `DICODE_RELAY_LOCAL_PORT` — webui port, where the task forwards inbound webhooks
- `DICODE_DATADIR` — daemon's data directory; storage backend roots blobs under `$DATADIR/relay-store/`
- `DICODE_STORAGE_TASK` (optional) — override the storage backend; defaults to `buildin/local-storage`

## Storage layout

| Blob | Storage key | Crypto context |
|---|---|---|
| Identity (PKCS8 sign + decrypt) | `relay/identity-v1` | `dicode/relay-identity/v1` |
| Broker TOFU pin | `relay/broker-pin-v1` | `dicode/relay-broker-pin/v1` |
