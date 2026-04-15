# OAuth Integration

dicode ships a built-in OAuth 2.0 system that handles the full authorization flow for 15 providers out of the box. Once authorized, tokens are stored as secrets and automatically refreshed — your tasks just read them from the environment.

Two flows are supported. Pick the one that matches your deployment:

| Flow | When to use | Requires |
|---|---|---|
| **Broker flow** (`buildin/auth-start`) | Default when the daemon is connected to a dicode relay. Zero OAuth app registration. | `relay.enabled: true` in `dicode.yaml` |
| **Local flow** (`auth/<provider>-oauth` tasks) | Self-hosted deployments, air-gapped installs, or when you want to use your own OAuth app | You register the OAuth app with the provider and set `<PROVIDER>_CLIENT_ID` / `_CLIENT_SECRET` secrets |

The rest of this document covers both. The broker flow is the simpler of the two and is the recommended default for developer machines.

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

The relay broker exchanges the authorization code with the provider, ECIES-encrypts the token bundle to your daemon's long-lived P-256 public key, and forwards the encrypted envelope over the existing WSS tunnel to `/hooks/oauth-complete`. The `buildin/auth-relay` built-in receives it, asks the daemon to decrypt via `dicode.oauth.store_token`, and writes the result to secrets.

Plaintext tokens never cross the JS runtime boundary — decrypt, parse, and `secrets.Set` all happen in Go-process memory, so a careless `console.log(input)` in a downstream task cannot leak credentials.

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
- **ECIES token delivery** — tokens are encrypted to the daemon's public key before leaving the broker process. Even a compromised relay operator or CDN cannot read them.
- **Type-as-AAD domain separation** — the envelope's message-type tag is bound into AES-GCM's authenticated data. A future ciphertext that reuses this same ECIES scheme under a different type label cannot be coaxed through the daemon's decrypt path.
- **Single-use pending sessions** — each `build_auth_url` call creates a session id that the daemon tracks and consumes on delivery. Unknown or expired sessions are rejected outright, which closes the chosen-salt oracle against the identity key.
- **Reserved delivery path** — the trigger engine refuses to bind `/hooks/oauth-complete` to any task other than `buildin/auth-relay`, which keeps a user task from accidentally (or maliciously) shadowing the delivery sink.
- **Audit log** — every successful delivery emits a structured metadata-only log entry (task, run, provider, session id, secret names written) so operators can trace incidents without the token ever reaching an observability pipeline.

### Task-level API

Two IPC primitives back the built-ins. You almost never call them directly — use `dicode run buildin/auth-start` and the built-in webhook task — but they are available to any task that declares the matching permission:

```yaml
permissions:
  dicode:
    oauth_init:  true   # grants dicode.oauth.build_auth_url
    oauth_store: true   # grants dicode.oauth.store_token
```

```ts
// build_auth_url: create a signed /auth/:provider URL
const { url, session_id } = await dicode.oauth.build_auth_url("slack", "channels:read");

// store_token: consume an incoming delivery envelope, decrypt in Go,
//              and write credentials to the secrets store.
const result = await dicode.oauth.store_token(input);
// result.secrets is the list of secret names written; plaintext stays in Go.
```

Both primitives are inert on daemons where `relay.enabled: false`, so the built-ins degrade cleanly when the relay is not configured.

### Failure modes

| Symptom | Cause |
|---|---|
| `oauth broker not configured on this daemon` | `relay.enabled: false`, or `BASE_URL` could not be derived from `relay.server_url`. |
| `unknown or expired session` | More than ~6 minutes elapsed between `build_auth_url` and the browser completing the flow; retry. |
| `decrypt failed` | Daemon restart between `build_auth_url` and the delivery (pending session was in memory), or the daemon's relay identity was rotated mid-flow. |
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

### 1. Open the task in the web UI

Navigate to the task for your provider (e.g. `auth/github-oauth`) and click **Run now**.

For providers that support **zero-setup** (Slack, Spotify, Linear, Salesforce, Discord, Confluence), dicode uses a shared built-in app — just click the authorize button and you're done.

### 2. Store your credentials (providers that require your own app)

```sh
dicode secret set GOOGLE_CLIENT_ID     <client-id>
dicode secret set GOOGLE_CLIENT_SECRET <client-secret>
```

Then run the task — it will redirect you to the provider's authorization screen.

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

To ensure a fresh token before a task runs, chain the OAuth task:

```yaml
# my-gmail-task/task.yaml
trigger:
  chain:
    from: auth/google-oauth
    on: success
permissions:
  env:
    - name: GOOGLE_ACCESS_TOKEN
      secret: GOOGLE_ACCESS_TOKEN
```

The OAuth task checks token validity first. If the token needs refreshing it silently rotates it and the chain runs with a fresh token. If re-authorization is needed, the chain fails with a desktop notification and a logged URL to open.

---

## Provider table

| Task ID | Provider | Flow | Token lifetime | Secrets to set |
|---------|----------|------|----------------|----------------|
| `auth/google-oauth` | Google | PKCE + secret | 1 h (auto-refreshed) | `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` |
| `auth/slack-oauth` | Slack | PKCE only | Permanent | `SLACK_CLIENT_ID` *(optional — built-in app works)* |
| `auth/github-oauth` | GitHub | PKCE + secret | Permanent | `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` |
| `auth/spotify-oauth` | Spotify | PKCE only | 1 h (auto-refreshed) | `SPOTIFY_CLIENT_ID` *(optional — built-in app works)* |
| `auth/linear-oauth` | Linear | PKCE only | Long-lived | `LINEAR_CLIENT_ID` *(optional — built-in app works)* |
| `auth/discord-oauth` | Discord | PKCE only | ~1 week (auto-refreshed) | `DISCORD_CLIENT_ID` *(optional — built-in app works)* |
| `auth/confluence-oauth` | Atlassian (Confluence / Jira) | PKCE only | 1 h (auto-refreshed) | `CONFLUENCE_CLIENT_ID` *(optional — built-in app works)* |
| `auth/salesforce-oauth` | Salesforce | PKCE only | Permanent | `SALESFORCE_CLIENT_ID` *(optional — built-in app works)* |
| `auth/airtable-oauth` | Airtable | PKCE + secret | 1 h (auto-refreshed) | `AIRTABLE_CLIENT_ID`, `AIRTABLE_CLIENT_SECRET` |
| `auth/gitlab-oauth` | GitLab | PKCE + secret | 2 h (auto-refreshed) | `GITLAB_CLIENT_ID`, `GITLAB_CLIENT_SECRET` |
| `auth/azure-oauth` | Microsoft Azure AD | PKCE + secret | 1 h (auto-refreshed) | `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET` |
| `auth/office365-oauth` | Office 365 / Microsoft Graph | PKCE + secret | 1 h (auto-refreshed) | `OFFICE365_CLIENT_ID`, `OFFICE365_CLIENT_SECRET` |
| `auth/notion-oauth` | Notion | Secret only | Permanent | `NOTION_CLIENT_ID`, `NOTION_CLIENT_SECRET` |
| `auth/stripe-oauth` | Stripe Connect | Secret only | Until revoked | `STRIPE_CLIENT_ID`, `STRIPE_SECRET_KEY` |
| `auth/looker-oauth` | Looker | PKCE (+ optional secret) | 1 h (no refresh) | `LOOKER_CLIENT_ID`, `LOOKER_INSTANCE` |

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
| Slack | `SLACK_USER_TOKEN` |
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
dicode secret set DICODE_BASE_URL https://dicode.mycompany.com
```

The OAuth tasks pick this up automatically and use it to build the redirect URI.
