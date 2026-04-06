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
dicoded -config dicode.yaml
```

On first start the daemon generates a P-256 keypair, stores it in the local
SQLite database, and logs the public webhook URL:

```
{"level":"info","msg":"relay connected","url":"https://relay.dicode.app/u/4a7b3c.../hooks/"}
```

**3. Copy the URL to your webhook provider:**

In GitHub → Settings → Webhooks, set the Payload URL to:

```
https://relay.dicode.app/u/<your-uuid>/hooks/github-push
```

That's it. The URL is stable across restarts as long as you keep your database.

---

## Config reference

```yaml
relay:
  enabled: true          # default: false
  server_url: wss://...  # relay WebSocket endpoint
```

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Enable the relay client |
| `server_url` | string | — | WebSocket URL of the relay server (must start with `wss://` in production) |

---

## Stable URL

The webhook URL is derived from a cryptographic keypair, not from your IP
address or hostname. As long as the dicode SQLite database is intact:

- Restarts do not change your URL.
- Changing networks does not change your URL.
- The relay reconnects automatically after network interruptions.

The URL has the form:

```
https://<relay-host>/u/<64-hex-chars>/hooks/
```

The 64-hex identifier is `sha256(public_key)`. It is stable, collision-resistant,
and cannot be guessed or squatted by another party.

---

## Exporting and backing up the relay key

The relay private key is stored in the SQLite database under the key
`relay.private_key` in the `kv` table. To back it up:

```sh
sqlite3 ~/.dicode/data.db "SELECT value FROM kv WHERE key = 'relay.private_key';"
```

To restore it on another machine, write the PEM to a temp file first, then
insert it — this avoids storing the raw key in your shell history:

```sh
# Export from source machine
sqlite3 ~/.dicode/data.db "SELECT value FROM kv WHERE key = 'relay.private_key';" > /tmp/relay_key.pem

# On the destination machine, import via a file variable (not an inline string)
PEM=$(cat /tmp/relay_key.pem)
sqlite3 ~/.dicode/data.db "INSERT OR REPLACE INTO kv (key, value) VALUES ('relay.private_key', '$PEM');"
rm /tmp/relay_key.pem
```

Keep this value secret. Anyone with the private key can impersonate your relay
client.

---

## Self-hosting the relay server

The relay server is included in the dicode repository (`pkg/relay/server.go`).
To run a standalone relay server, build and run the `relay-server` binary
(coming soon as a separate `cmd/relay-server`). Configure clients to point at
your server:

```yaml
relay:
  enabled: true
  server_url: wss://relay.example.com
```

The relay server requires no database; nonce state is kept in memory.
TLS termination should be handled by a reverse proxy (nginx, Caddy, etc.).
