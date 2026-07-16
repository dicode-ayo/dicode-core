# OAuth Broker Integration Test — Manual Runbook

End-to-end test of the relay broker OAuth flow between a local
`dicode-relay` (TypeScript) and `dicode daemon` (Go). Exercises the full
chain: mTLS handshake → `build_auth_url` → browser consent → Grant
callback → ECIES encrypt + broker sign → WSS delivery → daemon
decrypt + verify + `store_token` → secrets written.

This is a point-in-time manual verification runbook, not an automated
regression suite — run it after touching the relay OAuth wire protocol.

---

## Prerequisites

```
# Both repos checked out side by side, both on main
/workspaces/dicode-relay/
/workspaces/dicode-core/

# Node.js 22+, Go 1.25+, Deno (auto-installed by dicode on first run)

# A Slack OAuth app (or any provider — Slack is PKCE-only, simplest)
# Create at https://api.slack.com/apps → From Scratch
# OAuth redirect URL: http://localhost:5553/connect/slack/callback
# Bot Token Scopes: channels:read (or whatever you want)
# Copy the Client ID
```

---

## Step 1: Start the relay server

```bash
cd /workspaces/dicode-relay

# Create a minimal .env for local dev
cat > .env <<EOF
PORT=5553
MTLS_PORT=5554
BASE_URL=http://localhost:5553
SLACK_CLIENT_ID=<your-slack-client-id>
EOF

# Start the relay (auto-generates broker signing + mTLS server certs on first run)
node --env-file=.env dist/index.js
# Or if not built yet:
npm run build && node --env-file=.env dist/index.js
# Or for dev mode with auto-reload:
SLACK_CLIENT_ID=<id> npm run dev
```

**Expected output:**

```
broker: generated signing key at /workspaces/dicode-relay/broker-signing-key.pem
broker: generated mTLS server cert at /workspaces/dicode-relay/broker-mtls-cert.pem
dicode-relay public listener on port 5553
dicode-relay mTLS listener on port 5554
Base URL: http://localhost:5553
```

**Verify:** `curl http://localhost:5553/health` → `{"ok":true}`

---

## Step 2: Build and start the daemon

```bash
cd /workspaces/dicode-core

# Build
make build

# Ensure dicode.yaml points at the local relay
# (should already be set from the existing config)
grep -A4 'relay:' dicode.yaml
# relay:
#   enabled: true
#   server_url: wss://localhost:5554
#   broker_url: http://localhost:5553
#   ca_file: /workspaces/dicode-relay/broker-mtls-cert.pem

# Start the daemon
./dicode daemon
```

**Expected output (look for these lines):**

```
{"level":"info","msg":"relay connected","url":"http://localhost:5553/u/<your-uuid>/hooks/"}
{"level":"info","msg":"relay: stored broker signing key","pubkey":"MFkwEwYH…"}
```

The first line confirms the mTLS tunnel is up (the daemon verified the broker's
server cert against `ca_file` and presented its client cert). The second
confirms the broker's delivery-signing key was persisted.

**Note your UUID** from the `url` field — you'll need it later.

---

## Step 3: Start the OAuth flow

In a **separate terminal**:

```bash
cd /workspaces/dicode-core

# Trigger the auth-start builtin task
./dicode run buildin/auth-start provider=slack
```

**Expected output:**

```
OAuth flow started for slack.

Open this URL in a browser to authorize:

  http://localhost:5553/auth/slack?session=<uuid>&challenge=<b64>&relay_uuid=<hex>&sig=<b64>&timestamp=<ts>

Session: <session-uuid>
```

---

## Step 4: Complete the OAuth flow in a browser

1. Copy the URL from step 3 and open it in a browser.
2. The relay broker validates the daemon's ECDSA signature.
3. Browser redirects to Slack's OAuth consent page.
4. Approve the app.
5. Slack redirects back to `http://localhost:5553/connect/slack/callback?code=...`
6. Grant exchanges the code for a token.
7. The relay broker:
   - ECIES-encrypts the token to the daemon's pubkey
   - Signs the envelope with its broker signing key
   - Forwards via the WSS tunnel to `/hooks/oauth-complete`
8. Browser shows: "Authorization complete. You may close this tab."

---

## Step 5: Verify the token landed

```bash
# Check secrets
./dicode secrets list | grep SLACK
```

**Expected output:**

```
SLACK_ACCESS_TOKEN
SLACK_SCOPE
SLACK_TOKEN_TYPE
```

(Slack doesn't return `refresh_token` or `expires_in` for bot tokens,
so only these three appear.)

```bash
# Verify the access token works
./dicode secrets list   # just to see the names

# Check the daemon log for the audit entry
grep "oauth token delivered" ~/.dicode/daemon.log
```

**Expected log entry:**

```json
{"level":"info","msg":"oauth token delivered","task":"buildin/auth-relay","run":"<id>","provider":"slack","session":"<first-8-chars>","secrets":["SLACK_ACCESS_TOKEN","SLACK_SCOPE","SLACK_TOKEN_TYPE"]}
```

---

## Step 6: Verify broker signature enforcement

Test that a forged envelope is rejected. In a separate terminal:

```bash
# Craft a fake delivery and POST it directly to the daemon
curl -X POST http://localhost:8080/hooks/oauth-complete \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "oauth_token_delivery",
    "session_id": "00000000-0000-0000-0000-000000000000",
    "ephemeral_pubkey": "AAAA",
    "ciphertext": "BBBB",
    "nonce": "CCCC"
  }'
```

**Expected:** The request is rejected. The buildin/auth-relay task
runs but `store_token` fails because:
1. No valid `broker_sig` field → "delivery envelope missing broker_sig"
2. Even if you add a fake sig → "broker signature verification failed"
3. Even if you somehow bypass sig → "unknown or expired session"

Check the daemon run log to confirm:

```bash
./dicode logs <run-id-from-output>
```

---

## Step 7: Verify broker server-cert verification

Test that the daemon rejects a broker whose server cert is not trusted by
`relay.ca_file`.

```bash
# Stop the relay server (Ctrl-C)

# Delete the auto-generated mTLS server cert
rm /workspaces/dicode-relay/broker-mtls-cert.pem

# Restart the relay — it generates a NEW self-signed mTLS cert
node --env-file=.env dist/index.js
```

**Expected daemon behavior:** The relay client reconnects but the TLS
handshake fails, because the broker now presents a certificate the daemon's
`ca_file` does not trust. The daemon log should show a TLS verification error
(e.g. `x509: certificate signed by unknown authority`) and the connection is
not established.

**Recovery:**

```bash
# Point relay.ca_file at the new cert (self-signed broker),
# then restart the daemon.
#   relay:
#     ca_file: /workspaces/dicode-relay/broker-mtls-cert.pem

./dicode daemon
```

For the hosted relay, whose cert chains to a public CA, there is no `ca_file`
to update — a verification failure there means the platform trust store is out
of date.

---

## Step 8: Test re-auth (stale secret cleanup)

Run the flow again to verify stale secrets from a previous auth are
cleaned up:

```bash
./dicode run buildin/auth-start provider=slack scope="channels:read chat:write"
# Open the URL, authorize with different scopes
# Verify SLACK_SCOPE reflects the new scopes
./dicode secrets list | grep SLACK
```

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `oauth broker not configured on this daemon` | `relay.enabled: false` in dicode.yaml, or `server_url` has wrong scheme | Set `enabled: true` and `server_url: wss://localhost:5554` |
| `daemon not connected` on browser | Daemon hasn't connected the mTLS tunnel yet | Wait for `relay connected` in daemon log, then retry |
| TLS verification failure on connect (`certificate signed by unknown authority`) | Broker server cert not trusted, or its cert was regenerated | Set/update `relay.ca_file` to the broker's cert (self-signed), or ensure the system trust store is current (WebPKI) |
| Connection rejected, WS close 4401 | mTLS listener sits behind a TLS-terminating proxy that stripped the client cert | Expose the mTLS port directly or via an L4/TCP load balancer only |
| `invalid signature` at `/auth/slack` | Clock skew > 30s between relay and daemon | Sync clocks; check `timestamp` query param |
| `unknown provider: slack` | `SLACK_CLIENT_ID` not set in relay env | Add to `.env` and restart relay |
| `Encryption failed` at callback | Daemon disconnected between auth-start and callback | Ensure daemon stays connected; retry the flow |
| Token not appearing in secrets | Check daemon log for `store_token` errors | `./dicode logs <run-id>` on the auth-relay run |

---

## What this tests

| Layer | What's exercised |
|---|---|
| mTLS tunnel | Real mutual-TLS WebSocket handshake; broker derives the UUID from the client cert |
| Server-cert verification | Daemon verifies the broker cert via `ca_file` (self-signed) or WebPKI |
| ECDSA signing | Daemon signs `/auth/:provider` URL; broker verifies |
| PKCE binding | Challenge bound into signed payload |
| Grant OAuth | Real code exchange with Slack (or whichever provider) |
| ECIES encryption | Broker encrypts token to daemon pubkey |
| Type-as-AAD | Domain separation via GCM authenticated data |
| Broker signing | Broker signs envelope; daemon verifies |
| Pending sessions | Session created on build_auth_url, consumed on delivery |
| Secret storage | Token written to encrypted SQLite via secrets manager |
| Stale cleanup | Re-auth deletes previous secrets before writing new ones |
| Audit logging | Metadata-only log entry on successful delivery |

---

## Without a real OAuth provider (headless/CI)

For automated testing without a browser or real Slack app, you can
bypass Grant by directly hitting the relay's callback endpoint after
creating a session. This simulates what Grant does after the code
exchange.

```bash
# 1. Start auth flow normally
RESULT=$(./dicode run buildin/auth-start provider=slack 2>&1)
SESSION=$(echo "$RESULT" | grep "Session:" | awk '{print $2}')

# 2. Hit the relay's callback directly (bypasses Grant + Slack)
#    Grant's querystring transport means tokens arrive as query params.
curl "http://localhost:5553/callback/slack?access_token=xoxb-test-token-123&state=${SESSION}&scope=channels:read&token_type=bot"

# 3. Verify the test token landed
./dicode secrets list | grep SLACK
# → SLACK_ACCESS_TOKEN should contain xoxb-test-token-123
```

This works because:
- `state` in Grant maps to the session ID
- The callback handler looks up the session, encrypts, signs, and forwards
- The daemon doesn't care whether the token came from a real provider

**Note:** This bypass only works if Grant's middleware doesn't intercept
`/callback/slack` before the broker router does. In the current setup
with `transport: "querystring"`, Grant redirects TO the callback URL
with tokens as query params — so the broker router at
`GET /callback/:provider` receives them directly. Hitting it manually
with the same query params is functionally identical.
