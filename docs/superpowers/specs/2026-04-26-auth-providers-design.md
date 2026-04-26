# Auth providers dashboard — design

A new built-in task `buildin/auth-providers` exposes a webhook UI listing every
OAuth provider known to the user's dicode instance. For each provider it
shows connection state, expiry, scope, and a Connect / Reconnect button that
dispatches the corresponding per-provider auth task.

The design also adds one supporting daemon-side primitive,
`dicode.oauth.list_status()`, that lets *any* permission-gated task
introspect OAuth connection metadata without ever touching plaintext
tokens.

A second, larger feature — a generic "task contributes a webui sub-page" mechanism
that would let the auth-providers task (and future ones) surface as a first-class
nav entry inside the existing webui SPA — is **deferred** to a follow-up so
this PR stays focused. See [`docs/followups/auth-providers-webui-nav.md`](../../followups/auth-providers-webui-nav.md).

## User-visible outcome

- A new entry in the webui Tasks list: **buildin/auth-providers**. Clicking
  it opens the task-detail page, which links to `/hooks/auth-providers`.
- That URL serves a self-contained SPA showing one card per OAuth provider:
  - Brand label (e.g. "GitHub"), brand color, status pill
    (`Connected` / `Not connected` / `Expires in 42m` / `Expired`).
  - Default scopes the provider was granted (when known).
  - **Connect** / **Reconnect** button. Clicking dispatches the
    per-provider task (`auth/<provider>-oauth`) and opens the returned
    auth URL in a new tab. The card polls every 5 s and flips to
    **Connected** as soon as the relay broker delivers the token.
- The default provider list covers the 14 relay-broker providers shipped
  in the auth taskset plus OpenRouter (15 total). Users can subset via
  the task's `providers` param.

**Non-goals.** No disconnect / token revocation (provider-side revocation is
per-provider and not worth solving here). No editing of CLIENT_ID /
CLIENT_SECRET. No display of token *values* — only metadata. No iframe
embedding inside the existing webui shell yet (that's the follow-up).

## Architecture

```
┌── /hooks/auth-providers (new task) ──────────────────────────────┐
│ GET   /                          → index.html (SPA shell)        │
│ GET   /?action=list              → JSON: provider statuses       │
│ POST  / { action: "connect",                                     │
│          provider: "github" }    → JSON: { url, session_id }     │
└──────┬───────────────────────────┬───────────────────────────────┘
       │                           │
       ▼                           ▼
   list_status              run_task("auth/<p>-oauth")
       │                           │
┌──────┴───────────────────────────┴───────────────────────────────┐
│ daemon (Go)                                                      │
│  pkg/ipc/oauth_status.go   pkg/trigger → pkg/runtime/deno        │
│         │                                                        │
│         ▼                                                        │
│  pkg/secrets (encrypted SQLite + env fallback)                   │
└──────────────────────────────────────────────────────────────────┘
```

## Backend

### `pkg/ipc/oauth_status.go` (new)

A new IPC handler exposed as `dicode.oauth.list_status(providers: string[])`.
Caller-driven: the task supplies the list of provider names; the daemon
returns one status entry per input, in input order.

```go
package ipc

import (
    "context"
    "fmt"
    "github.com/dicode/dicode/pkg/secrets"
)

const maxStatusBatchSize = 64

type ProviderStatus struct {
    Provider  string  `json:"provider"`             // lowercase, as supplied
    HasToken  bool    `json:"has_token"`
    ExpiresAt *string `json:"expires_at,omitempty"` // RFC3339 string or absent
    Scope     *string `json:"scope,omitempty"`
    TokenType *string `json:"token_type,omitempty"`
}

func listOAuthStatus(ctx context.Context, mgr secrets.Manager, providers []string) ([]ProviderStatus, error) {
    if len(providers) > maxStatusBatchSize {
        return nil, fmt.Errorf("too many providers: %d > %d", len(providers), maxStatusBatchSize)
    }
    out := make([]ProviderStatus, 0, len(providers))
    for _, p := range providers {
        prefix, err := sanitizeProviderPrefix(p)
        if err != nil {
            return nil, fmt.Errorf("provider %q: %w", p, err)
        }
        // Resolve the four metadata keys; absence is fine and yields a
        // partial entry. Plaintext tokens are never read into ProviderStatus.
        access, _ := mgr.Resolve(ctx, prefix+"_ACCESS_TOKEN")
        entry := ProviderStatus{
            Provider: p,
            HasToken: access != "",
        }
        if v, _ := mgr.Resolve(ctx, prefix+"_EXPIRES_AT"); v != "" {
            entry.ExpiresAt = &v
        }
        if v, _ := mgr.Resolve(ctx, prefix+"_SCOPE"); v != "" {
            entry.Scope = &v
        }
        if v, _ := mgr.Resolve(ctx, prefix+"_TOKEN_TYPE"); v != "" {
            entry.TokenType = &v
        }
        out = append(out, entry)
    }
    return out, nil
}
```

`sanitizeProviderPrefix` is the existing helper at
[`pkg/ipc/oauth_store.go:128`](../../../pkg/ipc/oauth_store.go) — it
upper-cases, requires `[A-Z0-9_]` only, min 2 chars, no leading/trailing
underscore. Reusing it means an attacker who controls the `providers`
list cannot read arbitrary secret keys (e.g. `_ACCESS_TOKEN; rm -rf`).

The handler returns a `ProviderStatus` per input even when no token is
stored — `has_token: false` with the metadata pointers absent. This makes
the response shape predictable for the UI.

The handler **never reads** access tokens, refresh tokens, or any other
credential into the response. It reads `_ACCESS_TOKEN` only to compute
the boolean `HasToken`; the resolved string is discarded after the
emptiness check.

### Permission gate — `permissions.dicode.oauth_status`

[`pkg/task/spec.go`](../../../pkg/task/spec.go) `DicodePermissions`
gains:

```go
// OAuthStatus enables dicode.oauth.list_status().
// Returns connection-state metadata (presence flag, expiry, scope) for the
// provider names the caller passes — never plaintext tokens.
OAuthStatus bool `yaml:"oauth_status,omitempty" json:"oauth_status,omitempty"`
```

The IPC dispatcher (sibling of the existing `oauth_init` / `oauth_store`
gates) refuses the call when `OAuthStatus` is unset on the calling task's
spec. Default is denied.

### `pkg/runtime/deno/sdk/{shim.ts,sdk.d.ts}`

Extend the `dicode.oauth` namespace:

```ts
// sdk.d.ts
list_status(providers: string[]): Promise<ProviderStatus[]>;

interface ProviderStatus {
  provider:    string;
  has_token:   boolean;
  expires_at?: string;
  scope?:      string;
  token_type?: string;
}
```

Shim implementation calls the IPC primitive over the existing socket.

## Task — `tasks/buildin/auth-providers/`

```
tasks/buildin/auth-providers/
├── task.yaml          # webhook /hooks/auth-providers, auth: true
├── task.ts            # action handler: list, connect
├── task.test.ts       # mocked dicode.oauth + dicode.run_task
├── index.html         # SPA shell
└── app/
    ├── app.js         # entry, fetches list, wires Connect buttons
    ├── components/
    │   └── dc-provider-card.js
    ├── lib/
    │   └── api.js     # thin fetch wrapper
    └── theme.css      # imports the webui theme tokens
```

### `task.yaml`

```yaml
apiVersion: dicode/v1
kind: Task
name: "Auth Providers"
description: |
  Dashboard listing every OAuth provider known to this dicode instance,
  with connection state and a Connect button that runs the corresponding
  auth/<provider>-oauth task.

runtime: deno

trigger:
  webhook: /hooks/auth-providers
  auth: true

params:
  providers:
    type: string
    default: "github,google,slack,spotify,linear,discord,gitlab,airtable,notion,confluence,salesforce,stripe,office365,azure,openrouter"
    description: |
      Comma-separated provider keys to display. Override to subset.
      Each key must satisfy [a-z0-9_]{2,}.

permissions:
  dicode:
    oauth_status: true
    tasks:
      # Explicit list — `taskAllowed` (pkg/ipc/server.go:870) supports
      # only "*" or exact match, not glob, so each callable task ID is
      # listed by name. The set mirrors the entries in
      # tasks/auth/taskset.yaml plus the standalone openrouter task.
      - "auth/github-oauth"
      - "auth/google-oauth"
      - "auth/slack-oauth"
      - "auth/spotify-oauth"
      - "auth/linear-oauth"
      - "auth/discord-oauth"
      - "auth/gitlab-oauth"
      - "auth/airtable-oauth"
      - "auth/notion-oauth"
      - "auth/confluence-oauth"
      - "auth/salesforce-oauth"
      - "auth/stripe-oauth"
      - "auth/office365-oauth"
      - "auth/azure-oauth"
      - "auth/openrouter-oauth"

timeout: 30s

notify:
  on_success: false
  on_failure: false
```

### `task.ts` (sketch)

```ts
import type { DicodeSdk } from "../../sdk.ts";

interface ProviderMeta {
  key:    string;   // matches list_status `provider`
  label:  string;
  color:  string;   // brand colour
  taskId: string;   // task to run on Connect
}

const KNOWN: ProviderMeta[] = [
  { key: "github",   label: "GitHub",     color: "#24292e", taskId: "auth/github-oauth" },
  { key: "google",   label: "Google",     color: "#4285f4", taskId: "auth/google-oauth" },
  // ... 13 more
  { key: "openrouter", label: "OpenRouter", color: "#9b50e1", taskId: "auth/openrouter-oauth" },
];

export default async function main({ params, input, dicode, output }: DicodeSdk) {
  const requested = ((await params.get("providers")) ?? "").split(",").map(s => s.trim()).filter(Boolean);
  if (requested.length > 64) throw new Error("at most 64 providers");

  const inp = (input ?? null) as Record<string, unknown> | null;
  const action = (inp?.action ?? "list") as string;

  if (action === "list") {
    const statuses = await dicode.oauth.list_status(requested);
    const meta = new Map(KNOWN.map(m => [m.key, m]));
    return statuses.map(s => ({ ...s, meta: meta.get(s.provider) ?? null }));
  }

  if (action === "connect") {
    const p = String(inp?.provider ?? "");
    const m = KNOWN.find(k => k.key === p);
    if (!m) throw new Error(`unknown provider: ${p}`);
    const result = await dicode.run_task(m.taskId);
    return { provider: p, url: (result?.result as any)?.url, session_id: (result?.result as any)?.session_id };
  }

  throw new Error(`unknown action: ${action}`);
}
```

The KNOWN table is the *only* place provider metadata (label, colour,
mapped task ID) lives. Adding a provider means appending one row here
plus one taskset entry under `tasks/auth/`.

### `index.html` + `app/`

Minimal SPA. `index.html` includes `<script src="app/app.js" type="module">`.
`app.js` calls `fetch('?action=list')`, renders `<dc-provider-card>` per
result, polls every 5 s, and on click POSTs `action=connect&provider=<key>`,
then `window.open(url, '_blank')`.

The UI explicitly imports the existing webui theme variables
(`tasks/buildin/webui/app/theme.css` content copied or `@import`-ed) so
the cards look at home when the follow-up lands and the page is iframed.

## OpenRouter rename

[`tasks/auth/openrouter-oauth/task.{ts,yaml}`](../../../tasks/auth/openrouter-oauth)
writes `OPENROUTER_ACCESS_TOKEN` instead of `OPENROUTER_API_KEY`.
`expires_at`, `_REFRESH_TOKEN`, etc. remain absent — OpenRouter's
returned key is long-lived and not refreshable.

The task's description block is updated so the documented downstream
example becomes:

```yaml
env: [{ name: OPENAI_API_KEY, secret: OPENROUTER_ACCESS_TOKEN }]
```

No backwards-compat shim. dicode v0.1.0 is alpha, no production users
depend on the old name yet, and adding a dual-write path would have to
be reverted later.

## Data flow — Connect

```
1.  user clicks Connect on "github" card
2.  SPA → POST /hooks/auth-providers   { action: "connect", provider: "github" }
3.  task.ts looks up KNOWN["github"] → taskId "auth/github-oauth"
4.  dicode.run_task("auth/github-oauth")
       → returns { result: { url, session_id } }   (already the contract of the auth/github-oauth + auth-start tasks)
5.  task.ts → JSON { url, session_id }
6.  SPA → window.open(url, "_blank")
7.  user authorises in the new tab
8.  provider redirects to relay broker → /hooks/oauth-complete → buildin/auth-relay → secrets store
9.  SPA's 5 s poll picks up the new <P>_ACCESS_TOKEN and flips the card to "Connected"
```

`dicode.run_task` already requires the calling task to declare the
target task ID under `permissions.dicode.tasks`. The current
`taskAllowed` implementation supports only `"*"` or exact match
(no glob), so all 15 callable task IDs are listed explicitly in the
auth-providers `task.yaml` (see snippet above). Adding glob support
to `taskAllowed` is plausible but out of scope here — the explicit
list is a clearer audit trail anyway.

The `auth-relay` task (already shipped) is the receiver of the relay
broker's encrypted token delivery; it persists `<P>_ACCESS_TOKEN`,
`<P>_REFRESH_TOKEN`, `<P>_EXPIRES_AT`, `<P>_SCOPE`, `<P>_TOKEN_TYPE`
via `dicode.oauth.store_token`. No change there.

## Error handling

- `list_status` invalid-name error → daemon returns an IPC error; task.ts
  surfaces as a per-card error pill (`Unknown provider: …`). One bad
  name does not poison the whole batch — but `sanitizeProviderPrefix`
  is strict, so this is a developer-error case, not a runtime one.
- `list_status` over-cap → daemon errors before the loop runs;
  task.ts caps client-side at 64 too so this is double-defensive.
- `run_task` failure (e.g. relay client not configured for a
  broker-backed provider) → task.ts forwards the error message; SPA
  renders inline below the card.
- Iframe / asset paths: the trigger engine already blocks `..` traversal
  in static-asset paths
  ([`pkg/trigger/engine.go:843`](../../../pkg/trigger/engine.go)). No new
  asset-serving code is added.
- `run_task` returning a `result` without `url` → task.ts treats as a
  hard error and the SPA shows "Provider task did not return an auth URL".

## Security

- `dicode.oauth.list_status()` is opt-in via a new `OAuthStatus`
  permission, default-denied. Tasks without it cannot reach the handler.
- The handler reads only the metadata suffixes (`_EXPIRES_AT`, `_SCOPE`,
  `_TOKEN_TYPE`) into the response. `_ACCESS_TOKEN` is read only to set
  the boolean flag and is then discarded.
- `_REFRESH_TOKEN` is **never** read by `list_status`. It's a credential.
- `sanitizeProviderPrefix` rejects malformed names *before* any secret
  lookup, so a malicious `providers` value cannot escape into arbitrary
  secret-key namespaces (e.g. `_FOO`, `provider; key`, etc.).
- Batch size is capped at 64 to bound IPC work per call.
- `tasks/buildin/auth-providers/task.yaml` declares
  `trigger.auth: true` — the webhook is gated by the same login flow as
  every other authenticated webui-served task. Unauthenticated browsers
  redirect to `/login`.
- The SPA receives no token data; it sees only what `list_status`
  returned plus the `KNOWN` table.

## Tests

### `pkg/ipc/oauth_status_test.go`

Required cases (all required to pass before merge):

1. Empty `providers` list → empty response, no error.
2. One provider with full bundle stored → all four metadata fields
   populated; `has_token: true`.
3. One provider with only `_ACCESS_TOKEN` → `has_token: true`,
   metadata pointers absent.
4. One provider with no keys at all → `has_token: false`, all pointers
   absent.
5. Malformed name (`"a"`, `"_x"`, `"X;Y"`) → daemon error; no secret
   reads happen for any provider in the same batch.
6. Batch size > 64 → daemon error before loop.
7. Plaintext non-leakage: a stub `secrets.Manager` whose
   `Resolve("FOO_ACCESS_TOKEN")` returns a sentinel value (e.g.
   `"SENTINEL_PLAINTEXT_TOKEN"`) is wired into the handler. The
   JSON-marshalled response is byte-scanned to confirm the sentinel
   never appears, exercising the contract that `_ACCESS_TOKEN` is
   read only to set the boolean flag.

### `pkg/ipc/server_oauth_test.go` extension

A new test verifies the dispatcher refuses `oauth.list_status` calls when
the task spec's `Permissions.Dicode.OAuthStatus` is false. Mirrors the
existing tests for `oauth_init` and `oauth_store`.

### `tasks/buildin/auth-providers/task.test.ts`

1. `action=list` with two providers → calls `dicode.oauth.list_status`
   with the right array; merges with KNOWN and returns array of cards.
2. `action=connect` with a known provider → calls
   `dicode.run_task("auth/<p>-oauth")`; returns `{ provider, url,
   session_id }`.
3. `action=connect` with an unknown key → throws.
4. `params.providers` empty → returns empty list (no IPC call).
5. `params.providers` containing > 64 entries → throws.

### Playwright e2e

`tests/e2e/auth-providers.spec.ts`:

1. Navigate to `/hooks/auth-providers`. Mock daemon to return two
   providers (one connected with future `expires_at`, one not).
2. Assert two cards render with correct labels and pill states.
3. Click Connect on the disconnected card; intercept the POST and stub
   a `{ url }` response. Assert `window.open` was called with that URL.
4. Mock the next poll to flip the card to connected. Assert UI updates
   without manual reload.

Coverage target: same 90 % line target the rest of `pkg/ipc/` enforces.

## Out of scope (explicit, with rationale)

- **Disconnect / revoke.** Provider-side revocation is per-provider
  (Google's revoke endpoint, GitHub's app-settings page, etc.). Any
  meaningful disconnect would either delete local secrets without
  invalidating the upstream grant (silent footgun) or require N
  per-provider implementations. Out of scope; can be added later as a
  per-provider "Open provider settings" link.
- **Iframing inside the webui shell.** Captured in
  [`docs/followups/auth-providers-webui-nav.md`](../../followups/auth-providers-webui-nav.md).
- **Auto-refresh visibility.** The auth-relay task already auto-refreshes
  `<P>_ACCESS_TOKEN` when `<P>_EXPIRES_AT` approaches; the dashboard
  shows the latest stored expiry but doesn't currently expose refresh
  events.
- **`OPENROUTER_API_KEY` compat shim.** No alpha users; clean rename.

## Touched files (summary)

New:
- `pkg/ipc/oauth_status.go` (handler) + `pkg/ipc/oauth_status_test.go`
- `tasks/buildin/auth-providers/{task.yaml,task.ts,task.test.ts,index.html,app/...}`
- `tests/e2e/auth-providers.spec.ts`
- `docs/followups/auth-providers-webui-nav.md`

Modified:
- `pkg/task/spec.go` — add `OAuthStatus bool` to `DicodePermissions`
  next to the existing `OAuthInit` / `OAuthStore` fields
  ([`pkg/task/spec.go:267-273`](../../../pkg/task/spec.go)).
- `pkg/ipc/server.go` — add a new `CapOAuthStatus` constant alongside
  `CapOAuthInit` / `CapOAuthStore` (existing pattern at
  [`pkg/ipc/server.go:209-213`](../../../pkg/ipc/server.go)), populate
  it from `dp.OAuthStatus`, and add a dispatch case mirroring the
  `oauth_init` / `oauth_store` cases (around
  [`pkg/ipc/server.go:619`](../../../pkg/ipc/server.go) and
  [`pkg/ipc/server.go:673`](../../../pkg/ipc/server.go)) that gates on
  `hasCap(caps, CapOAuthStatus)`.
- `pkg/runtime/deno/sdk/{shim.ts,sdk.d.ts}` — typed `list_status`.
- `tasks/buildin/taskset.yaml` — register the new builtin entry.
- `tasks/auth/openrouter-oauth/task.ts` — rename `OPENROUTER_API_KEY` →
  `OPENROUTER_ACCESS_TOKEN`.
- `tasks/auth/openrouter-oauth/task.yaml` — rename in `permissions.env`
  and `env`.

Python SDK parity (`pkg/runtime/python/sdk/dicode_sdk.py`) is **out of
scope** for this PR — the Python SDK currently has no `dicode.oauth.*`
bindings at all, so adding `list_status` would mean introducing a new
namespace + IPC plumbing on the Python side, which deserves its own
design pass.

## Review cycle

Per the project's review-loop convention: after this lands as a PR, a
`/review` and `/security-review` cycle runs on the diff and iterates
inline until both reviewers are satisfied. Particular focus areas for
the security pass:

- `listOAuthStatus` never marshals access-token plaintext (test #7 above
  is the explicit assertion).
- `sanitizeProviderPrefix` rejects malformed names before any secret
  read.
- `permissions.dicode.oauth_status` defaults to denied and is checked at
  dispatch.
- `tasks/buildin/auth-providers/task.yaml` has `trigger.auth: true`.
- The Connect flow's `run_task` is constrained to the
  `permissions.dicode.tasks: ["auth/*"]` glob.
