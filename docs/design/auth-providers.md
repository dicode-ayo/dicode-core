# Auth providers dashboard — design

A built-in webhook task at `/hooks/auth-providers` that surfaces every OAuth
provider known to a dicode instance, with connection state, expiry, scope, and
a Connect button that orchestrates the appropriate authorisation flow.

Connection status is derived from a narrow, permission-gated primitive —
`dicode.secrets.has(<PROVIDER>_ACCESS_TOKEN)` — so the dashboard never touches
plaintext credentials.

## Architecture

```
┌── /hooks/auth-providers (built-in task) ────────────────────────┐
│  GET  /                          → index.html (Lit-based SPA)   │
│  POST { action: "list" }         → JSON: provider statuses      │
│  POST { action: "connect", p }   → JSON: { url, session_id? }   │
└──────┬──────────────┬────────────┬──────────────────────────────┘
       │              │            │
       ▼              ▼            ▼
  broker GET     secrets.has   run_task("buildin/auth-start")  ← relay providers
  /providers     per provider  or direct webhook URL           ← standalone / BYO
  (catalogue)    (status)
```

Broker-backed providers (github, google, slack, …) connect via
[`buildin/auth-start`](../../dicode-buildin/auth-start), which signs a
`/auth/:provider` URL with the daemon's relay identity and returns it.
OpenRouter is the only hardcoded standalone PKCE provider (no relay broker);
the dashboard opens its existing webhook directly. Any BYO entry instantiated
from the `_oauth-app` template is auto-discovered (see below) and opened via
its own webhook.

## Provider catalogue and status

The dashboard assembles its provider list from three sources at runtime:

1. **Broker catalogue** — `GET /providers` on the relay broker
   (`DICODE_RELAY_BROKER_URL`; requires dicode-relay ≥ 0.1.5) returns the
   live list of broker-backed providers with flow metadata only —
   `{key, pkce, scopes, secret_required, configured}`. No labels or brand
   colors; the SPA card falls back to the provider key and a neutral color
   (`meta.color` defaulting to `#888`) plus a per-key bundled SVG icon.
2. **Standalone table** — a small hardcoded map in
   [`task.ts`](../../dicode-buildin/auth-providers/task.ts) for providers
   that are neither broker-backed nor template-derived. Today that is only
   `openrouter`.
3. **BYO auto-discovery** — `dicode.list_tasks()` is scanned for tasks whose
   merged spec carries the `template: dicode.io/oauth-app` marker; each
   enabled inheritor with a webhook becomes a provider card automatically.

Per-provider connection status is a single presence check:
`dicode.secrets.has(<PROVIDER>_ACCESS_TOKEN)`. The check returns a boolean
and never reads the token value.

## Security model

1. **Plaintext tokens are never read.** Status is a `secrets.has` presence
   check; the token value never enters the task, the response, or the SPA.

2. **Narrow permission gates.** The task declares exactly what it needs in
   `permissions.dicode`: `secrets_has: true` (presence checks),
   `list_tasks: true` (BYO discovery), and a `tasks` allowlist containing
   only `buildin/auth-start`. None of these grants implies another, and
   none grants secret reads or writes.

3. **Best-effort status reads.** A failed `secrets.has` or `list_tasks`
   call is logged and degrades to `has_token: false` / an empty BYO list
   rather than failing the whole panel.

4. **Webhook auth.** The dashboard task itself declares `trigger.auth: true`,
   so unauthenticated browsers are gated by the dicode session wall.

## Connect flow

The dashboard's allowlist `permissions.dicode.tasks` contains exactly one
entry — `buildin/auth-start`. No other task is callable from the dashboard.

### Relay-broker providers (14 of them)

```
1.  user clicks Connect on a card (e.g. "github")
2.  SPA → POST /hooks/auth-providers   { action: "connect", provider: "github" }
3.  task.ts → dicode.run_task("buildin/auth-start", { provider: "github" })
4.  auth-start signs a /auth/:provider URL with the relay identity → returns { url, session_id }
5.  task.ts forwards { url, session_id } to the SPA; SPA opens url in a new tab
6.  user authorises with the provider; relay broker delivers the encrypted token
    to /hooks/oauth-complete; buildin/auth-relay decrypts and persists
    <P>_ACCESS_TOKEN/_REFRESH_TOKEN/_EXPIRES_AT/_SCOPE/_TOKEN_TYPE
7.  SPA's 5 s poll picks up the new metadata and flips the card to "Connected"
```

The per-provider `auth/<p>-oauth` tasks are NOT called directly from the
dashboard — they return HTML via `handleAuthNeeded`, not a JSON
`{ url, session_id }` contract, so they cannot be invoked programmatically.
`auth-start` is the canonical "give me a signed URL" entry point.

### OpenRouter (standalone PKCE)

OpenRouter does its own PKCE handshake against `openrouter.ai`, no relay
broker involved. The dashboard short-circuits `run_task` and returns the
provider's webhook URL directly:

```
{ "provider": "openrouter", "url": "${DICODE_BASE_URL}/hooks/openrouter-oauth" }
```

The user clicks through to that page, completes the authorisation, and
the openrouter task persists `OPENROUTER_ACCESS_TOKEN`. OpenRouter shares
the `<P>_ACCESS_TOKEN` naming convention so the dashboard sees its status
identically to the broker-backed providers (rename landed in PR #221).

## Provider metadata

The broker's `GET /providers` response supplies flow metadata only (`key`,
`pkce`, `scopes`, `secret_required`, `configured`) — adding a broker provider
requires no dashboard change, but its card carries no label or brand color
from the broker; the SPA falls back to the provider key and a neutral color.
Standalone providers live in the small `STANDALONE` map in
[`task.ts`](../../dicode-buildin/auth-providers/task.ts) (currently only
openrouter), and BYO entries carry their own label/color metadata via
`_oauth-app` params (`color`, task name), picked up by the `list_tasks`
scan.

## Out of scope (explicit, with rationale)

- **Disconnect / revoke.** Provider-side revocation is per-provider (Google's
  revoke endpoint, GitHub's app-settings page, etc.). Any meaningful
  disconnect would either delete local secrets without invalidating the
  upstream grant (silent footgun) or require N per-provider implementations.
  Can be added later as a per-provider "Open provider settings" link.
- **Generic "task contributes a webui sub-page" mechanism.** The dashboard
  is reachable only via the existing webui's task-list drilldown. A first-
  class nav entry inside the webui SPA is tracked at
  [#222](https://github.com/dicode-ayo/dicode-core/issues/222).
- **Auto-refresh visibility.** The auth-relay task already auto-refreshes
  `<P>_ACCESS_TOKEN` when `<P>_EXPIRES_AT` approaches; the dashboard shows
  the latest stored expiry but doesn't currently expose refresh events.
- **Slack token-key mismatch with the legacy local-PKCE path.** Fixed in
  [#223](https://github.com/dicode-ayo/dicode-core/issues/223): the token
  key is now `SLACK_ACCESS_TOKEN`, consistent with the
  `<PROVIDER_UPPER>_ACCESS_TOKEN` convention the dashboard expects.
