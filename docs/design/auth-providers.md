# Auth providers dashboard — design

A built-in webhook task at `/hooks/auth-providers` that surfaces every OAuth
provider known to a dicode instance, with connection state, expiry, scope, and
a Connect button that orchestrates the appropriate authorisation flow.

The dashboard is built around one supporting daemon-side primitive,
`dicode.oauth.list_status()`, that lets any permission-gated task introspect
OAuth connection metadata without ever touching plaintext credentials.

## Architecture

```
┌── /hooks/auth-providers (built-in task) ────────────────────────┐
│  GET  /                          → index.html (Lit-based SPA)   │
│  POST { action: "list" }         → JSON: provider statuses      │
│  POST { action: "connect", p }   → JSON: { url, session_id? }   │
└──────┬───────────────────────────┬──────────────────────────────┘
       │                           │
       ▼                           ▼
   list_status              run_task("buildin/auth-start")    ← relay providers
                            or direct webhook URL              ← OpenRouter (standalone)
       │
┌──────┴──────────────────────────────────────────────────────────┐
│  daemon (Go)                                                    │
│   pkg/ipc/oauth_status.go   →  secrets.Chain.Resolve            │
│                                (env-fallback aware)             │
└─────────────────────────────────────────────────────────────────┘
```

The 15 broker-backed providers (github, google, slack, …) connect via
[`buildin/auth-start`](../../tasks/buildin/auth-start), which calls
`dicode.oauth.build_auth_url` and returns a signed `/auth/:provider` URL.
OpenRouter is the only standalone PKCE provider (no relay broker); the dashboard
opens its existing webhook directly.

## The `dicode.oauth.list_status` primitive

```go
// pkg/ipc/oauth_status.go
type ProviderStatus struct {
    Provider  string  `json:"provider"`             // lowercase, as supplied
    HasToken  bool    `json:"has_token"`
    ExpiresAt *string `json:"expires_at,omitempty"` // RFC3339 or absent
    Scope     *string `json:"scope,omitempty"`
    TokenType *string `json:"token_type,omitempty"`
}

func listOAuthStatus(ctx context.Context, chain secrets.Chain, providers []string) ([]ProviderStatus, error)
```

Reads `<P>_ACCESS_TOKEN`/`_EXPIRES_AT`/`_SCOPE`/`_TOKEN_TYPE` for each
caller-supplied provider name through the daemon's `secrets.Chain`. The chain
walks the env-var-fallback provider, so a token set via the host environment
shows up as connected just like one written to the encrypted local store.

**Why `secrets.Chain` and not `secrets.Manager`:** Manager is the write-side
CRUD interface (List/Set/Delete) and has no read method.
[`Chain.Resolve`](../../pkg/secrets/provider.go) is the right interface for
read-with-env-fallback semantics.

## Security model

The handler enforces several invariants that together make the primitive safe
to expose to any opt-in task:

1. **Plaintext tokens are never read into the response.** The handler reads
   `<P>_ACCESS_TOKEN` only to set the boolean `HasToken` flag and discards the
   value. `_REFRESH_TOKEN` is never read. The unit test
   `TestListOAuthStatus_PlaintextNonLeakage` proves this with a sentinel byte
   scan against the marshalled response.

2. **Provider names are sanitised before any secret-key lookup.** The shared
   helper `sanitizeProviderPrefix` ([`pkg/ipc/oauth_store.go`](../../pkg/ipc/oauth_store.go))
   accepts only `[A-Z0-9_]{2,}` with no leading/trailing underscore, so a
   malicious caller cannot escape into arbitrary secret-key namespaces.

3. **Permission gate.** A new `OAuthStatus bool` field on
   [`task.DicodePermissions`](../../pkg/task/spec.go) opts a task into the
   primitive. The IPC dispatcher checks
   `hasCap(caps, CapOAuthStatus)` before invocation; the cap is independent
   of the existing `OAuthInit` (build_auth_url) and `OAuthStore`
   (store_token) caps, and a task with one does not implicitly get the
   others.

4. **Context-aware error propagation.** `resolveOrEmpty` tolerates
   `NotFoundError` and transient backend errors as empty values (status reads
   are best-effort), but propagates `context.Canceled` /
   `context.DeadlineExceeded` so cancelled requests fail fast instead of
   silently returning false-negative `has_token: false` rows for every
   remaining provider.

5. **Batch size cap (64).** Bounds the per-call work — each provider triggers
   up to four secret reads.

6. **Webhook auth.** The dashboard task itself declares `trigger.auth: true`,
   so unauthenticated browsers are gated by the dicode session wall.

## Connect flow

The dashboard's allowlist `permissions.dicode.tasks` contains exactly one
entry — `buildin/auth-start`. No other task is callable from the dashboard.

### Relay-broker providers (14 of them)

```
1.  user clicks Connect on a card (e.g. "github")
2.  SPA → POST /hooks/auth-providers   { action: "connect", provider: "github" }
3.  task.ts → dicode.run_task("buildin/auth-start", { provider: "github" })
4.  auth-start calls dicode.oauth.build_auth_url(provider) → returns { url, session_id }
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

The dashboard's hardcoded `KNOWN` table in [`task.ts`](../../tasks/buildin/auth-providers/task.ts)
is the single source of UI metadata (label, brand color, standalone-ness).
Adding a provider requires a row here and a corresponding entry in
[`tasks/auth/taskset.yaml`](../../tasks/auth/taskset.yaml). The two lists
are intentionally co-located in similar shapes; if drift becomes painful,
extract a shared `providers.json` consumed by both.

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
- **Slack token-key mismatch with the legacy local-PKCE path.** The
  `slack-oauth` task overrides `access_token_env: SLACK_USER_TOKEN`, so
  legacy-path tokens won't show as Connected on the dashboard. Tracked at
  [#223](https://github.com/dicode-ayo/dicode-core/issues/223).
