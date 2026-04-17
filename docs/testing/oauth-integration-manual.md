# OAuth Broker Integration Test — Manual Runbook

End-to-end test of the relay broker OAuth flow between a local
`dicode-relay` (TypeScript) and `dicode daemon` (Go). Exercises the full
chain: WSS handshake → `build_auth_url` → browser consent → Grant
callback → ECIES encrypt + broker sign → WSS delivery → daemon
decrypt + verify + `store_token` → secrets written.

---

## Prerequisites

```
# Both repos checked out side by side
/workspaces/dicode-relay/    # feat/oauth-aad-domain-sep branch
/workspaces/dicode-core/     # feat/oauth-relay-client branch

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
BASE_URL=http://localhost:5553
SLACK_CLIENT_ID=<your-slack-client-id>
EOF

# Start the relay (auto-generates broker signing key on first run)
node --env-file=.env dist/index.js
# Or if not built yet:
npm run build && node --env-file=.env dist/index.js
# Or for dev mode with auto-reload:
SLACK_CLIENT_ID=<id> npm run dev
```

**Expected output:**

```
broker: generated signing key at /workspaces/dicode-relay/broker-signing-key.pem
dicode-relay listening on port 5553
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
grep -A2 'relay:' dicode.yaml
# relay:
#   enabled: true
#   server_url: ws://localhost:5553

# Start the daemon
./dicode daemon
```

**Expected output (look for these lines):**

```
{"level":"info","msg":"relay connected","url":"http://localhost:5553/u/<your-uuid>/hooks/"}
{"level":"info","msg":"relay: pinned broker signing key (trust-on-first-use)","pubkey":"MFkwEwYH…"}
```

The first line confirms the WSS tunnel is up. The second confirms
TOFU pinning of the broker's signing key.

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

## Step 7: Verify TOFU broker key pinning

Test that the daemon rejects a relay with a different signing key.

```bash
# Stop the relay server (Ctrl-C)

# Delete the auto-generated broker key
rm /workspaces/dicode-relay/broker-signing-key.pem

# Restart the relay — it generates a NEW key
node --env-file=.env dist/index.js
```

**Expected daemon behavior:** The relay client reconnects but the
handshake fails because the new broker pubkey doesn't match the
pinned one. The daemon log should show:

```
relay: BROKER PUBKEY CHANGED — the relay server presented a different
signing key than the one pinned on first connect. If the relay operator
rotated their key, run `dicode relay trust-broker --yes` to accept the
new key. Connection rejected to prevent token substitution attacks.
```

**Recovery:**

```bash
./dicode relay trust-broker --yes
# → Broker pubkey pin cleared. Restart the daemon to accept the new broker key.

# Restart the daemon
# (or just wait — the relay client will reconnect and re-pin automatically)
```

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
| `oauth broker not configured on this daemon` | `relay.enabled: false` in dicode.yaml, or `server_url` has wrong scheme | Set `enabled: true` and `server_url: ws://localhost:5553` |
| `daemon not connected` on browser | Daemon hasn't connected the WSS tunnel yet | Wait for `relay connected` in daemon log, then retry |
| `invalid signature` at `/auth/slack` | Clock skew > 30s between relay and daemon | Sync clocks; check `timestamp` query param |
| `unknown provider: slack` | `SLACK_CLIENT_ID` not set in relay env | Add to `.env` and restart relay |
| `Encryption failed` at callback | Daemon disconnected between auth-start and callback | Ensure daemon stays connected; retry the flow |
| `BROKER PUBKEY CHANGED` on reconnect | Broker key was regenerated (new .pem file) | Run `dicode relay trust-broker --yes` then restart daemon |
| Token not appearing in secrets | Check daemon log for `store_token` errors | `./dicode logs <run-id>` on the auth-relay run |

---

## What this tests

| Layer | What's exercised |
|---|---|
| WSS tunnel | Real WebSocket handshake with ECDSA challenge-response |
| ECDSA signing | Daemon signs `/auth/:provider` URL; broker verifies |
| PKCE binding | Challenge bound into signed payload |
| Grant OAuth | Real code exchange with Slack (or whichever provider) |
| ECIES encryption | Broker encrypts token to daemon pubkey |
| Type-as-AAD | Domain separation via GCM authenticated data |
| Broker signing | Broker signs envelope; daemon verifies |
| TOFU pinning | Daemon pins broker pubkey on first connect |
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
