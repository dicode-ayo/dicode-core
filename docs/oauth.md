# OAuth Integration

dicode handles the full OAuth 2.0 authorization flow for any provider. Once authorized, tokens are stored as secrets and automatically refreshed — your tasks just read them from the environment.

Two flows are supported. Pick the one that matches your deployment:

| Flow | When to use | Requires |
|---|---|---|
| **Broker flow** (`buildin/auth-start`) | Default when the daemon is connected to a dicode relay. The broker hosts shared OAuth app credentials for the providers it knows about (github, slack, google, spotify, linear, discord, gitlab, airtable, notion, confluence, salesforce, stripe, office365, azure as of dicode-relay@0.1.5). | `relay.enabled: true` in `dicode.yaml` |
| **BYO flow** (instantiate `auth/_oauth-app` from your own taskset) | Self-hosted / air-gapped installs, providers the broker doesn't carry (e.g. Looker), or any provider you'd rather drive with your own OAuth app. Auto-discovered by the dashboard via the `template: dicode.io/oauth-app` marker — no per-task config in this repo. | You register an app with the provider and set `<PROVIDER>_CLIENT_ID` / `<PROVIDER>_CLIENT_SECRET` secrets |

The rest of this document covers both. The broker flow is the simpler of the two and is the recommended default for developer machines.

> **Migration note (2026-05).** Earlier dicode releases shipped per-provider entries `auth/github-oauth`, `auth/google-oauth`, `auth/slack-oauth`, etc. for every broker-backed provider. Those entries were removed once the broker became the single source of truth — the broker flow above replaces them. Operators who relied on `/hooks/<provider>-oauth` callbacks for broker-backed providers must move to the broker flow (no callback URL re-registration needed) or instantiate `_oauth-app` themselves with their own provider config (see the BYO walkthrough in [tasks/auth/taskset.yaml](../tasks/auth/taskset.yaml)).

---

## Broker flow (relay required)

When the relay client is enabled in `dicode.yaml`, two built-in tasks handle the OAuth dance end-to-end. You do not register an app with the provider — the relay operator has already done that for 14+ providers.

### 1. Start the flow

```sh
dicode run buildin/auth-start provider=slack
```

The task prints a signed `/auth/slack` URL. Open it in a browser and complete the provider's consent screen. The URL is valid for about a minute and bound to your daemon's relay identity — no one else can complete the flow on your behalf.

Optional scope override:

```sh
dicode run buildin/auth-start provider=slack scope="channels:read chat:write"
```

### 2. Wait for the token delivery

The relay broker exchanges the authorization code with the provider, ECIES-encrypts the token bundle to your daemon's long-lived P-256 decryption public key, and forwards the encrypted envelope over the existing WSS tunnel to `/hooks/oauth-complete`. The `buildin/auth-relay` built-in receives it, verifies the broker signature (`broker_sig`) against the TOFU-pinned broker key, ECIES-decrypts the envelope in-task, and writes each credential to the secrets store via `dicode.secrets_set`.

Decryption happens inside a deliberately locked-down task: `silent: true` discards stdout/stderr (so nothing token-shaped can reach log capture), `permissions.net` and `permissions.fs` are empty (no outbound network, no disk writes), and only a minimal set of env vars is exposed. The only output channels are `dicode.secrets_set` and the storage task, which sees only ciphertexts.

### 3. Consume the token

After delivery, the following secrets are populated under a naming convention derived from the provider:

| Secret | Meaning |
|---|---|
| `<PROVIDER>_ACCESS_TOKEN` | Access token. Always present. |
| `<PROVIDER>_REFRESH_TOKEN` | Refresh token, if the provider returned one. |
| `<PROVIDER>_EXPIRES_AT` | RFC3339 expiry timestamp, if the provider returned `expires_in`. |
| `<PROVIDER>_SCOPE` | Granted scopes, if the provider returned a scope string. |
| `<PROVIDER>_TOKEN_TYPE` | Token type (`Bearer`, `bot`, etc.), if provided. |

`<PROVIDER>` is the provider key upper-cased (`SLACK`, `GITHUB`, `GOOGLE`, …).

Inject the token into your task like any other secret:

```yaml
# tasks/my-slack-bot/task.yaml
permissions:
  env:
    - name: SLACK_TOKEN
      secret: SLACK_ACCESS_TOKEN
```

```ts
// tasks/my-slack-bot/task.ts
const token = Deno.env.get("SLACK_TOKEN")!;
const res = await fetch("https://slack.com/api/auth.test", {
  headers: { Authorization: `Bearer ${token}` },
});
```

### Security model

- **ECDSA-signed initiation** — the `/auth/:provider` URL is signed by the daemon's P-256 identity key. The broker verifies the signature against the pubkey it knows for that UUID (from the live WSS registry) before starting the flow.
- **PKCE binding in the signed payload** — the broker's own challenge is cryptographically bound to the daemon that initiated the flow, preventing challenge-swap hijacks.
- **ECIES token delivery** — tokens are encrypted to the daemon's decryption public key before leaving the broker process. Even a compromised relay operator or CDN cannot read them.
- **Type-as-AAD domain separation** — the envelope's message-type tag is bound into AES-GCM's authenticated data. A future ciphertext that reuses this same ECIES scheme under a different type label cannot be coaxed through the daemon's decrypt path.
- **Single-use pending sessions** — each `auth-start` invocation creates a session id and persists an encrypted `{session → provider}` record; `auth-relay` consumes and deletes the record on delivery. Unknown or expired sessions are rejected outright.
- **Reserved delivery path** — the trigger engine refuses to bind `/hooks/oauth-complete` to any task other than `buildin/auth-relay`, which keeps a user task from accidentally (or maliciously) shadowing the delivery sink.
- **Audit log** — every successful delivery emits a structured metadata-only log entry (task, run, provider, session id, secret names written) so operators can trace incidents without the token ever reaching an observability pipeline.

### Task-level API

There is no dedicated OAuth IPC surface — the built-ins compose the flow
from generic, independently gated primitives. You almost never touch these
directly (use `dicode run buildin/auth-start` and the built-in webhook
task), but they are what the tasks declare:

```yaml
permissions:
  dicode:
    crypto:                            # dicode.crypto.encrypt / decrypt,
      - "dicode/relay-identity/v1"     # scoped to the listed contexts
      - "dicode/oauth-pending/v1"
    tasks:                             # dicode.run_task allowlist —
      - buildin/local-storage          # the encrypted-blob storage backend
    secrets_write: true                # dicode.secrets_set (auth-relay only)
    secrets_has:   true                # dicode.secrets.has (auth-providers only)
```

- `dicode.crypto.encrypt(ctx, bytes)` / `dicode.crypto.decrypt(ctx, bytes)` —
  XChaCha20-Poly1305 (context bound into the AEAD's associated data), with
  context-scoped sub-key derivation from the secrets master key. `auth-start`
  and `auth-relay` use it for the relay identity, the broker pin, and
  pending-session records; a task can only use contexts listed under
  `permissions.dicode.crypto`.
- `dicode.secrets_set(name, value)` — writes a secret. Gated by
  `secrets_write`; only `buildin/auth-relay` holds it in this flow.
- `dicode.secrets.has(name)` — presence check without reading the value.
  Gated by `secrets_has`; used by the `auth-providers` dashboard.
- `dicode.run_task(task, params)` — reaches the storage task for
  encrypted-at-rest blobs, allowlisted per task via
  `permissions.dicode.tasks`.

Each grant is independent; granting one does not grant the others. If the
relay is not configured, `buildin/auth-start` fails fast with a clear error
(see below) and the local flow remains available.

### Failure modes

| Symptom | Cause |
|---|---|
| `relay broker URL not configured (DICODE_RELAY_BROKER_URL)` | Relay disabled, or no broker URL could be derived from `relay.server_url` (an explicit `relay.broker_url` overrides the derivation). |
| `unknown or expired session` | More than ~5 minutes (the broker's session TTL) elapsed between `auth-start` and the browser completing the flow; retry. |
| `broker signature verification failed` | The delivery was signed by a broker key that no longer matches the TOFU pin. |
| `decrypt failed` | The daemon's relay identity was rotated mid-flow. |
| `daemon not connected` | The WSS tunnel was not open when the browser hit `/auth/:provider`. Start the daemon first, wait for the `relay connected` log line, then run `auth-start`. |

---

## Local flow (self-hosted, no broker)

If you run your own dicode instance without the relay — or you want to use your own OAuth app for a specific provider — the original local-task flow is still fully supported. Each provider is a **webhook task** that implements the OAuth flow end-to-end on your daemon.

### How it works

Each provider is a **webhook task** that implements the OAuth flow:

```
Browser                   dicode                    Provider
  │                          │                          │
  │  GET /hooks/google-oauth │                          │
  │ ─────────────────────── ▶│                          │
  │                          │ 1. Generate PKCE verifier│
  │                          │    Store in KV store     │
  │◀ ── Redirect to auth URL ┤                          │
  │                          │                          │
  │ ─────────────────────────────────────────────────── ▶  Login + consent
  │◀ ─────────────────────────────────────────── ?code=...  Redirect back
  │                          │                          │
  │  GET /hooks/google-oauth?code=... ─────────────────▶│
  │                          │ 2. Exchange code         │
  │                          │    Store tokens as secrets
  │◀─────────── Success page ┤                          │
```

**Subsequent runs** check whether the stored token is still valid:

- **Token valid** → return immediately (used by chain triggers for token checks)
- **Token expired + refresh token** → refresh silently, update secret, continue
- **No token / expired without refresh** → show authorization button again

---

## Quick start

### 1. Open the dashboard

Navigate to **Tasks → Auth Providers** in the web UI (or visit `/hooks/auth-providers` directly). Each provider gets a row with a **Connect** button. Connect for broker-backed providers fires `buildin/auth-start`; Connect for openrouter opens its standalone webhook page; Connect for any BYO entry you instantiated from `_oauth-app` opens that entry's webhook.

For broker-backed providers (github, slack, google, spotify, linear, discord, gitlab, airtable, notion, confluence, salesforce, stripe, office365, azure) you do NOT need to register your own OAuth app — the broker has one. Just click Connect.

### 2. Store your credentials (BYO providers only)

For any provider you've added to your own taskset by instantiating `_oauth-app`:

```sh
dicode secrets set MY_PROVIDER_CLIENT_ID     <client-id>
dicode secrets set MY_PROVIDER_CLIENT_SECRET <client-secret>   # if needed
```

Then click Connect — it will redirect you to the provider's authorization screen.

### 3. Use the token in your tasks

After authorization, the token is stored as a secret and injected as an environment variable:

```typescript
// task.ts
export default async function main({ dicode }: DicodeSdk) {
  const token = Deno.env.get("GOOGLE_ACCESS_TOKEN");
  const res = await fetch("https://gmail.googleapis.com/gmail/v1/users/me/messages", {
    headers: { Authorization: `Bearer ${token}` },
  });
  // ...
}
```

```yaml
# task.yaml
permissions:
  env:
    - name: GOOGLE_ACCESS_TOKEN
      secret: GOOGLE_ACCESS_TOKEN
```

### 4. Automate token refresh with chain triggers

To ensure a fresh token before a task runs, chain the OAuth task. For broker-backed providers, chain from `buildin/auth-start`. For BYO providers, chain from the BYO entry in your taskset:

```yaml
# my-task/task.yaml — chain after a BYO _oauth-app entry you instantiated
trigger:
  chain:
    from: my-tasks/my-looker-oauth   # your own BYO entry
    on: success
permissions:
  env:
    - name: MY_LOOKER_ACCESS_TOKEN
      secret: MY_LOOKER_ACCESS_TOKEN
```

The OAuth task checks token validity first. If the token needs refreshing it silently rotates it and the chain runs with a fresh token. If re-authorization is needed, the chain fails with a desktop notification and a logged URL to open.

### 5. Automate first-time setup with `if_missing`

Chain triggers are great for keeping existing tokens fresh, but they assume the token is already there. For *first-run* setup — e.g. a user opens the chat UI for the first time and there's no `OPENROUTER_ACCESS_TOKEN` stored yet — attach an `if_missing:` directive directly to the env entry:

```yaml
# ai-agent-openrouter preset
permissions:
  env:
    - name: OPENROUTER_ACCESS_TOKEN
      secret: OPENROUTER_ACCESS_TOKEN
      if_missing:
        task: auth/openrouter-oauth
```

Behavior on dispatch:

1. Engine checks whether `OPENROUTER_ACCESS_TOKEN` resolves from the secrets store.
2. Present → main task runs immediately.
3. Missing → engine synchronously fires `auth/openrouter-oauth` in chain mode. If the prereq completes and the secret is now present, the main task runs. If the prereq throws with an authorize URL, that error becomes the main task's failure — the UI surfaces a clickable setup link.

The same task doubles as the setup flow and the silent refresh path; once the secret is stored, `if_missing` is a no-op and subsequent dispatches skip straight to the main task. Chain triggers (#4 above) and `if_missing` compose — chain for ongoing refresh of a known-good token, `if_missing` for the one-time setup that happens before there's anything to refresh.

See [task-format.md § permissions.env](concepts/task-format.md#permissionsenv--environment-variables) for the full form reference.

---

## Dashboard

The built-in `auth-providers` task at `/hooks/auth-providers` provides a
single-page dashboard for managing every OAuth provider connection at
once. It lists each provider's connection state (Connected, Not
connected, Expires in 42m, Expired), shows the granted scope when
known, and offers a Connect / Reconnect button per row that kicks off
the appropriate OAuth flow:

- **Relay-broker providers** (github, google, slack, …): clicking
  Connect runs `buildin/auth-start` which returns a signed
  `/auth/:provider` URL; the dashboard opens it in a new tab.
- **OpenRouter** (the only standalone PKCE provider): clicking
  Connect opens `/hooks/openrouter-oauth` directly, where the user
  clicks an "Authorize with OpenRouter" button.

Once the token lands in the secrets store, the next 5 s poll flips the
card to "Connected" automatically.

Reach the dashboard via the webui task list (Tasks → Auth Providers →
"open webhook UI"), or directly at `http://localhost:8080/hooks/auth-providers`.

The dashboard never exposes plaintext tokens. Connection status is
determined via `dicode.secrets.has(<PROVIDER>_ACCESS_TOKEN)` — a presence
check that never reads the token value — and the provider catalogue comes
from the broker's `GET /providers` endpoint.

---

## Provider table

The authoritative provider list comes from two sources at runtime: the relay broker's `GET /providers` endpoint (live list — query it via the dashboard) and the local `auth/taskset.yaml` entries below.

### Broker-backed (`buildin/auth-start`)

The relay broker handles these providers — no per-provider task entry needed locally. Connect from the dashboard or run `dicode run buildin/auth-start provider=<key>`. The full live list is at `<broker>/providers`; the snapshot below is current as of `dicode-relay@0.1.5`.

| Provider | Flow | Token lifetime |
|---|---|---|
| `github` | PKCE + secret | Permanent |
| `slack` | PKCE only | Permanent |
| `google` | PKCE + secret | 1 h (auto-refreshed) |
| `spotify` | PKCE only | 1 h (auto-refreshed) |
| `linear` | PKCE only | Long-lived |
| `discord` | PKCE only | ~1 week (auto-refreshed) |
| `gitlab` | PKCE + secret | 2 h (auto-refreshed) |
| `airtable` | PKCE + secret | 1 h (auto-refreshed) |
| `notion` | Secret only | Permanent |
| `confluence` | PKCE only | 1 h (auto-refreshed) |
| `salesforce` | PKCE only | Permanent |
| `stripe` | Secret only | Until revoked |
| `office365` | PKCE + secret | 1 h (auto-refreshed) |
| `azure` | PKCE + secret | 1 h (auto-refreshed) |

### Local `auth/taskset.yaml` entries (standalone)

| Task ID | Provider | Flow | Token lifetime | Secrets to set |
|---|---|---|---|---|
| `auth/openrouter-oauth` | OpenRouter | PKCE, no client registration | Until revoked (API key) | *(none — zero setup)* |

### BYO entries (auto-discovered)

For any provider you instantiate from `auth/_oauth-app/task.yaml` in your own taskset, the dashboard discovers it via the `template: dicode.io/oauth-app` marker — no entry to add to either table above. See the walkthrough in `tasks/auth/taskset.yaml` header for the full pattern (Looker, self-hosted GitLab, niche enterprise IdPs, etc.).

### Flow types explained

| Flow | Client secret needed | PKCE | Notes |
|------|---------------------|------|-------|
| **PKCE only** | No | Yes | Safest for desktop/local apps. No secret to store or leak. |
| **PKCE + secret** | Yes | Yes | Provider requires a client secret in addition to PKCE. |
| **Secret only** | Yes | No | Provider doesn't support PKCE (e.g. Notion). |

### Stored secrets per provider

After a successful authorization the following secrets are written:

| Provider | Secrets written |
|----------|----------------|
| Google | `GOOGLE_ACCESS_TOKEN`, `GOOGLE_REFRESH_TOKEN` |
| Slack | `SLACK_ACCESS_TOKEN` |
| GitHub | `GITHUB_ACCESS_TOKEN` |
| Spotify | `SPOTIFY_ACCESS_TOKEN`, `SPOTIFY_REFRESH_TOKEN` |
| Linear | `LINEAR_ACCESS_TOKEN` |
| Discord | `DISCORD_ACCESS_TOKEN`, `DISCORD_REFRESH_TOKEN` |
| Atlassian | `CONFLUENCE_ACCESS_TOKEN`, `CONFLUENCE_REFRESH_TOKEN` |
| Salesforce | `SALESFORCE_ACCESS_TOKEN`, `SALESFORCE_INSTANCE_URL` |
| Airtable | `AIRTABLE_ACCESS_TOKEN`, `AIRTABLE_REFRESH_TOKEN` |
| GitLab | `GITLAB_ACCESS_TOKEN`, `GITLAB_REFRESH_TOKEN` |
| Azure AD | `AZURE_ACCESS_TOKEN`, `AZURE_REFRESH_TOKEN` |
| Office 365 | `OFFICE365_ACCESS_TOKEN`, `OFFICE365_REFRESH_TOKEN` |
| Notion | `NOTION_ACCESS_TOKEN` |
| Stripe Connect | `STRIPE_ACCESS_TOKEN`, `STRIPE_REFRESH_TOKEN`, `STRIPE_ACCOUNT_ID` |
| Looker | `LOOKER_ACCESS_TOKEN` |

---

## Adding a custom provider

The OAuth system is built on a generic task (`tasks/auth/_oauth-app/`) driven entirely by `taskset.yaml` overrides. To add a new provider, add an entry to your `taskset.yaml`:

```yaml
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: auth
spec:
  entries:
    my-service-oauth:
      ref:
        path: tasks/auth/_oauth-app/task.yaml
      overrides:
        name: My Service OAuth
        trigger:
          webhook: /hooks/my-service-oauth
        params:
          provider:          my-service        # key in _oauth-app/providers.ts
          scope:             "read write"
          token_lifetime:    expires            # or: permanent
          color:             "#FF6600"
          client_id_env:     CLIENT_ID
          client_secret_env: CLIENT_SECRET      # omit for PKCE-only
          access_token_env:  MY_SERVICE_ACCESS_TOKEN
          refresh_token_env: MY_SERVICE_REFRESH_TOKEN
        env:
          - { name: CLIENT_ID,                      secret: MY_SERVICE_CLIENT_ID }
          - { name: CLIENT_SECRET,                  secret: MY_SERVICE_CLIENT_SECRET }
          - { name: MY_SERVICE_ACCESS_TOKEN,  secret: MY_SERVICE_ACCESS_TOKEN,  optional: true }
          - { name: MY_SERVICE_REFRESH_TOKEN, secret: MY_SERVICE_REFRESH_TOKEN, optional: true }
        net:
          - auth.my-service.com
```

Then register the provider in `tasks/auth/_oauth-app/providers.ts`:

```typescript
import * as P from "../_oauth/providers.ts";

// In the PROVIDERS map:
"my-service": {
  provider: P.MyService,   // add to _oauth/providers.ts
  name: "My Service",
  redirectSuffix: "my-service-oauth",
},
```

And add the provider config to `tasks/auth/_oauth/providers.ts`:

```typescript
export const MyService: OAuthProvider = {
  authUrl:  "https://auth.my-service.com/oauth/authorize",
  tokenUrl: "https://auth.my-service.com/oauth/token",
};
```

---

## Token lifetime and refresh behaviour

| `token_lifetime` param | Behaviour |
|------------------------|-----------|
| `expires` | Checks KV-stored expiry timestamp. Refreshes automatically if within 60 seconds of expiry. Falls back to re-auth if no refresh token. |
| `permanent` | Skips all expiry and refresh logic. Returns immediately if a token is already stored. |

The expiry timestamp is stored in the task's KV store under `<provider>_oauth_expires_at` (Unix milliseconds). It is set from the `expires_in` field in the token response.

---

## Redirect URI

The redirect URI is always:

```
http://localhost:8080/hooks/<provider>-oauth
```

If your dicode instance runs on a different host or port, set the `DICODE_BASE_URL` secret:

```sh
dicode secrets set DICODE_BASE_URL https://dicode.mycompany.com
```

The OAuth tasks pick this up automatically and use it to build the redirect URI.
