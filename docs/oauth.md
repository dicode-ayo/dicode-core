# OAuth Integration

dicode ships a built-in OAuth 2.0 system that handles the full authorization flow for 15 providers out of the box. Once authorized, tokens are stored as secrets and automatically refreshed — your tasks just read them from the environment.

---

## How it works

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
