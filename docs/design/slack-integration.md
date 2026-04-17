# Slack Connection — Design

## Scope

This document covers **one question only**: how does a dicode user connect a
Slack app to their dicode instance so that Slack can reach it (slash commands,
events, interactivity payloads)? Three approaches are compared end-to-end,
with the broker-side and daemon-side changes each would require, what the user
has to do manually, and the trade-offs.

**Not in scope:**
- What tasks look like on the Slack side, how Slack payloads are dispatched
  to tasks, what a task-author-facing contract (if any) should be, how
  modals/inputs/outputs are represented. All of that is a separate design
  and can be discussed independently once the connection layer is settled.
- Any other chat platform (Discord, Teams). Some of the patterns here
  generalize, but this doc is strictly about Slack.
- Replacing the existing `auth-start` / `auth-relay` flow for user-token
  Slack access (the current broker entry `slack` with `channels:read`
  scope). That path stays as-is; this doc is about a separate concern.

## Background: what already exists

Before proposing changes, it's worth being precise about what's already in
place, because a lot of the groundwork is done:

- **Relay tunnel.** [`pkg/relay/client.go`](../../pkg/relay/client.go) in dicode-core connects outbound
  over WSS to the relay server; inbound HTTP requests at
  `https://relay.dicode.app/u/<uuid>/hooks/<path>` are forwarded down the
  tunnel as `request` messages and answered with `response` messages. The
  routing key at the relay edge is the `<uuid>` in the URL path. No broker
  involvement, no lookup — purely URL-driven.
- **OAuth broker.** [`dicode-relay/src/broker`](../../../dicode-relay/src/broker/) holds registered `client_id`/
  `client_secret` pairs for a set of providers ([`providers.ts`](../../../dicode-relay/src/broker/providers.ts)) and runs
  the full authorization-code flow on behalf of users via Grant middleware.
  On completion it ECIES-encrypts the delivered tokens against the daemon's
  P-256 public key and forwards them as a `request` to `/hooks/oauth-complete`
  on the target daemon.
- **auth-start / auth-relay tasks.** [`tasks/buildin/auth-start`](../../tasks/buildin/auth-start/) prints an
  authorize URL; [`tasks/buildin/auth-relay`](../../tasks/buildin/auth-relay/) is the reserved sink at
  `/hooks/oauth-complete` that decrypts the envelope in Go memory and writes
  secrets via the `dicode.oauth.store_token` IPC primitive. Plaintext tokens
  never cross the JS runtime.
- **Slack is already a broker provider**, currently registered with
  `channels:read` user-token scopes and no client_secret (PKCE-only). This
  is fine for user-token reads; it is **not** what a slash-command bot
  needs.

What does *not* exist today:

- Any path for Slack to deliver **inbound events** (slash commands,
  interactivity, event subscriptions) to a dicode daemon.
- Any broker-side role beyond OAuth.
- Any `team_id`-to-instance mapping.

The three approaches below differ in how they fill that gap.

## Approach A — Per-user Slack app, direct to relay URL

**TL;DR:** The user creates their own Slack app at api.slack.com, sets its
Request URL to their personal relay endpoint, and pastes the bot token and
signing secret into dicode as secrets. The broker is not involved. Fully
self-hosted; maximum manual effort.

### Architecture

```
┌─────────┐     HTTPS POST     ┌────────────────────────┐
│  Slack  │ ─────────────────▶ │ relay.dicode.app/u/... │
└─────────┘                    └───────────┬────────────┘
                                            │  existing WSS tunnel
                                            ▼
                                   ┌────────────────┐
                                   │ user's dicode  │
                                   └────────────────┘
```

The Request URL the user puts in their Slack app is an ordinary dicode
webhook URL, e.g.
`https://relay.dicode.app/u/<uuid>/hooks/slack-events`. Slack POSTs flow
through the existing tunnel unchanged — the relay doesn't know or care that
the traffic is Slack-shaped. Everything downstream of the URL is "just a
webhook" from dicode's current perspective.

### What's required to ship this

**Broker side (dicode-relay):** nothing. The relay tunnel is already capable
of carrying Slack POSTs; no new endpoint, no new provider, no config.

**Daemon side (dicode-core):** nothing, if you're willing to have Slack
requests land at an ordinary webhook path and be handled by whatever task
the user binds there. If you want a reserved path (so that only a specific
builtin or trusted task can handle Slack events), add path reservation in
[`pkg/trigger/engine.go`](../../pkg/trigger/engine.go) analogous to how `/hooks/oauth-complete` is
reserved today — but that's a policy choice, not a blocker for Approach A.

### What the user does (one-time, ~5 minutes)

1. Visit <https://api.slack.com/apps> → **Create New App** → **From scratch**.
2. Pick a name and workspace.
3. **OAuth & Permissions** → Bot Token Scopes → add whatever scopes the
   intended use needs (e.g. `commands`, `chat:write`).
4. **Slash Commands** and/or **Interactivity & Shortcuts** and/or **Event
   Subscriptions** — wherever Slack asks for a Request URL, paste the user's
   own `https://relay.dicode.app/u/<uuid>/hooks/slack-events`.
5. **Install to Workspace**, approve.
6. Copy the **Bot User OAuth Token** (`xoxb-…`) and the **Signing Secret**.
7. In dicode:
   ```
   dicode secrets set slack_bot_token <xoxb-…>
   dicode secrets set slack_signing_secret <signing secret>
   ```

After this, Slack POSTs reach the daemon. What happens next is not the
concern of this document.

### Trade-offs

**Pros:**
- Zero code changes anywhere. Ships today against the current codebase.
- Fully self-hosted. The user owns the Slack app, the tokens, the daemon,
  and the routing decision. No central infrastructure.
- Simplest trust model: Slack → the user's own relay URL → the user's
  dicode. No third party on the path.
- A compromised or suspended Slack app affects only one user.

**Cons:**
- ~5 minutes of manual clicking through Slack's UI per user. Friction for
  onboarding, especially for users who don't know what "Bot Token Scopes"
  or "signing secret" mean.
- No "install from marketplace" experience.
- Scope or redirect changes require going back into api.slack.com.
- The user has to know their relay URL ahead of time and paste it in.

### Missing pieces

Approach A itself has no missing pieces on the dicode side — it's the "do
nothing, document the manual steps" option. The only deliverable is a
well-written how-to guide. Worth stating explicitly: this is a deployment
model, not a feature.

---

## Approach B — Shared `dicode` Slack app, broker-mediated

**TL;DR:** The dicode project publishes one Slack app centrally. Users
install it via OAuth (same mechanism as `auth-start provider=github` today),
and the broker routes incoming Slack events to the right dicode daemon by
looking up `team_id → relay_uuid`. Best UX, meaningful broker-side work,
expands the broker's responsibilities.

### Architecture

Two phases: install, then steady-state.

**Install phase** (piggybacks on the existing OAuth broker flow):

```
┌──────┐            ┌──────────────┐           ┌─────────┐
│ user │            │ dicode-core  │           │ broker  │
└──┬───┘            └──────┬───────┘           └────┬────┘
   │ auth-start             │                        │
   │ provider=slack-bot     │                        │
   │───────────────────────▶│                        │
   │                        │ build_auth_url         │
   │                        │───────────────────────▶│
   │                        │◀───────────────────────│
   │◀── print URL ──────────│                        │
   │                        │                        │
   │ click, approve in Slack                         │
   │ ────────────────────────────────────────────▶  Slack
   │                                                  │
   │                        │           ┌─────────── ▼ ───┐
   │                        │◀──── ECIES-encrypted ───────│
   │                        │      { access_token,        │
   │                        │        team_id: "T123" }    │
   │                        │                             │
   │                        │  /hooks/oauth-complete      │
   │                        │  (existing auth-relay sink) │
   │                        │                             │
   │                        │  SLACK_BOT_TOKEN +          │
   │                        │  SLACK_TEAM_ID in secrets   │
   │                        │                             │
   │                        │ reconnect relay WSS,        │
   │                        │ announce slack_team_ids     │
   │                        │ in hello bindings           │
   │                        │───────────────────────────▶ │
   │                        │   broker adds T123 → uuid   │
   │                        │   to in-memory registry     │
```

**Steady state** (for every subsequent Slack POST):

```
┌─────────┐   POST /slack/events   ┌────────┐
│  Slack  │ ─────────────────────▶ │ broker │
└─────────┘                        └───┬────┘
                                       │ 1. verify HMAC w/ SLACK_SIGNING_SECRET
                                       │ 2. parse team_id from payload
                                       │ 3. lookup team_id → relay_uuid
                                       │ 4. ack Slack (200) immediately
                                       │ 5. forward body over WSS tunnel
                                       ▼
                                 ┌──────────────┐
                                 │ user's dicode│
                                 └──────────────┘
                                       │
                                       ▼
                              /hooks/slack-events
```

The broker becomes the **one public endpoint** the Slack app targets. Every
user's Slack events land on the same URL; the broker routes them by
`team_id` lookup.

### Broker-side changes required

**1. Register a separate `slack-bot` provider.** The existing `slack` entry
in [`providers.ts`](../../../dicode-relay/src/broker/providers.ts) uses user-token scopes. Don't change it; add a new
entry so both flavors coexist. Roughly:

```ts
["slack-bot", {
  grantKey: "slack",
  clientIdEnv: "SLACK_BOT_CLIENT_ID",
  secretEnv: "SLACK_BOT_CLIENT_SECRET",
  pkce: true,
  scopes: ["commands", "chat:write"],
}]
```

Cost: ~10 lines.

**2. Propagate `team.id` through the OAuth delivery envelope.** Grant's
normalized Slack response puts `team: { id, name }` in its `raw` field.
The broker needs to extract `team.id` during token exchange and include it
in the ECIES-encrypted delivery payload. The envelope schema gains one
optional field:

```ts
interface OAuthTokenDeliveryPayload {
  provider: string;
  access_token: string;
  refresh_token?: string;
  expires_at?: number;
  extras?: Record<string, string>;   // e.g. { slack_team_id: "T123" }
}
```

Keeping this as a generic `extras` map (rather than a Slack-specific field)
is deliberate — the same mechanism lets future providers surface other
side-channel identifiers (`stripe_user_id`, `notion_workspace_id`, etc.)
without another protocol bump. Cost: ~30 lines across the grant wrapper,
envelope schema, and router glue.

**3. Extend the relay `hello` handshake with daemon bindings.** This is the
load-bearing architectural piece. The broker needs a `team_id → uuid` map
to do steady-state routing, and the broker's design rule is "no persistent
storage — all state ephemeral in-memory with TTLs." To honor that rule,
**the daemon announces its Slack bindings in every handshake**:

```ts
interface HelloMessage {
  type: "hello";
  uuid: string;
  pubkey: string;
  sig: string;
  timestamp: number;
  // NEW:
  bindings?: {
    slack_team_ids?: string[];
  };
}
```

The daemon reads `SLACK_TEAM_ID` from its secrets at connect time, wraps it
in `bindings.slack_team_ids`, and sends it with the signed hello. The
broker adds `team_id → uuid` to an in-memory map on handshake accept, and
removes it on disconnect. On daemon restart, the binding is re-announced
automatically. On broker restart, every reconnecting daemon rebuilds its
row.

This keeps the broker stateless across restarts, matches the existing
"ephemeral-only" philosophy, and avoids any new persistent store. The cost
is one protocol field and a `SlackRegistry` class on the broker side
(~50 lines of TypeScript total). The field is optional, so daemons that
don't have Slack configured simply omit it — backwards compatible.

**4. Add `POST /slack/events` endpoint.** New route in [`src/broker/router.ts`](../../../dicode-relay/src/broker/router.ts):

```ts
router.post("/slack/events", async (req, res) => {
  if (!verifySlackSignature(req.rawBody, req.headers, env.SLACK_SIGNING_SECRET)) {
    return res.status(401).send();
  }
  const teamId = extractTeamId(req.body);
  const uuid = slackRegistry.lookup(teamId);
  if (!uuid) return res.status(404).send();

  // Slack's 3-second deadline: ack immediately, forward asynchronously.
  res.status(200).send();
  relayServer.forward(uuid, {
    method: "POST",
    path: "/hooks/slack-events",
    headers: req.headers,
    body: req.rawBody,
  }).catch(logger.error);
});
```

Two subtleties worth flagging:
- **3-second deadline.** Slack requires HTTP 200 within 3 seconds on slash
  commands and view_submissions. The tunnel round-trip to a daemon may or
  may not fit — it's safer to ack immediately and forward in the
  background. The daemon replies later via `response_url`, which is the
  standard Slack pattern and works fine with the existing tunnel semantics.
- **Signature verification at the edge.** The broker holds the Slack app's
  signing secret (one env var, `SLACK_SIGNING_SECRET`) and verifies every
  inbound payload once. Daemons trust the broker to have verified — they
  don't need the signing secret at all. This is consistent with how the
  existing tunnel handles trust: what comes down the WSS from the broker
  is already vouched for.

Cost: ~80 lines including the signature helper and the forward wrapper.

**5. New broker env vars.** `SLACK_BOT_CLIENT_ID`, `SLACK_BOT_CLIENT_SECRET`
(the OAuth credentials for the shared app), and `SLACK_SIGNING_SECRET`
(for inbound event verification). Add to [`.env.example`](../../../dicode-relay/.env.example) and
[`docs/providers.md`](../../../dicode-relay/docs/providers.md).

**6. Route mount.** Make sure `/slack/events` is at the top of the broker's
router, not nested under `/auth/...` or `/callback/...`, so the Slack app's
Request URL can be set to `https://relay.dicode.app/slack/events`.

**7. Slack app registration.** The dicode project has to register one
public Slack app at api.slack.com, mark it as **Distribute to any
workspace** (otherwise only the dev-org workspace can install), and fill in
`SLACK_BOT_CLIENT_ID` / `SLACK_BOT_CLIENT_SECRET` / `SLACK_SIGNING_SECRET`
in the broker env. One-time, but a real step.

### Daemon-side changes required

1. **Read `SLACK_TEAM_ID` from secrets and include it in the hello message.**
   Small change to [`pkg/relay/client.go`](../../pkg/relay/client.go) hello construction, ~20 lines.
2. **Accept and persist the `extras` field from OAuth delivery.** Extend
   [`pkg/relay/oauth.go`](../../pkg/relay/oauth.go) and the `dicode.oauth.store_token` IPC primitive
   in [`pkg/ipc/server.go`](../../pkg/ipc/server.go) to write each `extras[key]` entry as a named
   secret. ~30 lines.
3. **Optional: reserved `/hooks/slack-events` webhook path** so arbitrary
   user tasks can't bind it. Same policy question as Approach A; independent
   of approach choice.

### What the user does

```
$ dicode auth-start provider=slack-bot
OAuth flow started for slack-bot.
Open this URL in a browser to authorize:
  https://relay.dicode.app/auth/slack-bot?session_id=…&sig=…

[user clicks, approves in Slack]

Secrets written: SLACK_BOT_TOKEN, SLACK_TEAM_ID
```

One command, one browser approval, no api.slack.com visit, no token paste.

### Missing pieces

- `slack-bot` provider entry in broker [`providers.ts`](../../../dicode-relay/src/broker/providers.ts)
- `extras` field in OAuth envelope + grant wrapper + protocol schemas
- `bindings` field in relay hello + matching Zod schema in [`src/relay/protocol.ts`](../../../dicode-relay/src/relay/protocol.ts)
- `SlackRegistry` class in broker (in-memory map, handshake listeners)
- `POST /slack/events` route with signature verification
- Broker unit tests for signature verification, team_id lookup, unknown-team
  404, forward-to-tunnel round-trip (use in-process RelayServer harness as
  existing tests do)
- Daemon-side hello extension, `extras` handling in oauth_store, matching
  Go-side unit tests
- Protocol version bump and documentation updates in
  [`docs/design/relay.md`](relay.md) and the broker's protocol docs
- One public Slack app registered at api.slack.com by the dicode project

### Trade-offs

**Pros:**
- One command (`auth-start`) installs everything. Nothing to copy-paste.
- Natural path to a marketplace listing.
- Credentials managed centrally in the broker, same pattern as GitHub,
  Google, Notion.
- The handshake-announces-bindings approach keeps the broker stateless —
  no new database, no persistence.

**Cons:**
- Expands the broker's role from "OAuth broker" to "OAuth broker + inbound
  Slack event router." Real architectural shift, worth discussing before
  committing.
- One dedicated public Slack app per bot flavor. If later additions (a
  user-token Slack bot, a DM-only bot, etc.) need different scopes or
  personalities, they need separate broker provider entries and possibly
  separate routing endpoints.
- Shared app = shared blast radius. If the dicode Slack app is ever
  suspended or rate-limited by Slack, every user loses Slack connectivity
  simultaneously.
- Protocol version bump on the relay handshake. Backwards compatible
  (new field is optional), but still a coordination point between
  broker and daemon releases.
- Pattern doesn't obviously generalize. Discord, Linear, GitHub App each
  want their own signature scheme and their own routing key, so a second
  provider means another broker endpoint and another routing table. See
  Open Questions.

---

## Approach C — Task-driven manifest install

**TL;DR:** A builtin dicode task uses Slack's [`apps.manifest.create`](https://api.slack.com/methods/apps.manifest.create)
API to programmatically create a personal Slack app in the user's workspace,
pre-filled with their relay URL, producing an install link. Collapses
Approach A's manual clicks into one command while preserving "you own the
app." Reuses the existing auth-relay webhook pattern — no localhost port, no
browser automation.

### Architecture

```
┌──────┐       ┌──────────────┐         ┌────────┐         ┌────────┐
│ user │       │ dicode-core  │         │ broker │         │ Slack  │
└──┬───┘       └──────┬───────┘         └───┬────┘         └───┬────┘
   │                  │                      │                  │
   │ run slack-install                        │                  │
   │─────────────────▶│                      │                  │
   │                  │ build_auth_url       │                  │
   │                  │ provider=slack-config│                  │
   │                  │─────────────────────▶│                  │
   │◀── print URL ────│                      │                  │
   │                  │                      │                  │
   │ click, approve config-token scope                          │
   │────────────────────────────────────────────────────────────▶
   │                  │                      │                  │
   │                  │                      │◀── code ─────────│
   │                  │                      │ exchange         │
   │                  │                      │─────────────────▶│
   │                  │                      │◀── config_token ─│
   │                  │                      │                  │
   │                  │◀── ECIES envelope ───│                  │
   │                  │   (via /hooks/       │                  │
   │                  │    oauth-complete)   │                  │
   │                  │                      │                  │
   │                  │ SLACK_APP_CONFIG_TOKEN in secrets       │
   │                  │                      │                  │
   │                  │ chain-triggered task:│                  │
   │                  │ POST apps.manifest   │                  │
   │                  │ .create with manifest│                  │
   │                  │ template (Request URL│                  │
   │                  │  = user's relay URL) │                  │
   │                  │─────────────────────────────────────────▶
   │                  │◀── app_id + install URL ────────────────│
   │◀── print install │                      │                  │
   │    URL ──────────│                      │                  │
   │                  │                      │                  │
   │ click, approve install                                     │
   │────────────────────────────────────────────────────────────▶
   │                  │                      │                  │
   │ (continues with Approach A's runtime flow:                 │
   │  Slack posts go to user's relay URL.                       │
   │  OR continues with Approach B's runtime flow:              │
   │  Slack posts go to broker's /slack/events.                 │
   │  Which one depends on the Request URL in the manifest.)    │
```

Key insight: **Approach C is not standalone.** It is a programmatic
installer that produces either an Approach-A app (manifest points at the
user's own relay) or an Approach-B-equivalent app (manifest points at the
broker's `/slack/events`, though in practice if you're using Approach B
you'd use its native auth-start flow instead). In practice, Approach C is
the "Approach A auto-installer."

### Why a task, not a CLI command

Because the flow needs to catch an OAuth redirect, and dicode already has
exactly the right machinery for that: the `auth-relay` reserved webhook
path. The install task doesn't bind a local port, doesn't run a local HTTP
server, doesn't call `xdg-open`. It uses the same "print URL → user
approves → broker delivers encrypted token → chain task resumes" pattern
the codebase already uses for every other OAuth provider.

### Broker-side changes required

**1. Register a `slack-config` provider.** Slack's config-token OAuth is a
separate Slack OAuth client from the bot app — it grants the
`apps.manifest:write` scope, which is used only to create and update Slack
apps. It expires every 12 hours, which is fine for a one-time install.

```ts
["slack-config", {
  grantKey: "slack",
  clientIdEnv: "SLACK_CONFIG_CLIENT_ID",
  secretEnv: "SLACK_CONFIG_CLIENT_SECRET",
  pkce: true,
  scopes: ["apps.manifest:write"],
}]
```

Cost: ~10 lines.

**2. Register one Slack "configuration token" OAuth client at api.slack.com**
by the dicode project. One-time, separate from the Approach B public app
(though the same dev org can own both).

That is the entirety of the broker-side work for Approach C. It reuses the
existing auth-start / auth-relay path with no protocol changes.

### Daemon-side changes required

**1. Two builtin tasks** in [`tasks/buildin/`](../../tasks/buildin/):

- `slack-install-begin` — manual trigger. Calls
  `dicode.oauth.build_auth_url("slack-config")`, prints the authorize URL,
  returns. Essentially a thin wrapper over `auth-start` that hardcodes the
  provider. Could alternatively be expressed as "run auth-start with
  provider=slack-config," in which case this task is unnecessary.

- `slack-install-complete` — **chain trigger** on `buildin/auth-relay`
  success, filtered to `provider=slack-config`. Reads `SLACK_APP_CONFIG_TOKEN`
  from secrets, generates a manifest (JSON with placeholders for app name,
  Request URL, bot scopes), POSTs to `slack.com/api/apps.manifest.create`,
  and prints the install URL from Slack's response.

**2. Manifest template** embedded in the task. Small JSON literal with
substituted values for `name`, `request_url`, `bot_scopes`, and
optionally `home_tab_enabled`. The Request URL is the user's own relay URL
(`https://relay.dicode.app/u/<uuid>/hooks/slack-events`), read from
`dicode.get_config()` at task run time.

**3. Chain-trigger filter by OAuth provider.** The existing chain trigger
system fires on task completion. The install-complete task needs to
distinguish between `provider=slack-config` (resume install) and any other
provider (ignore). This can be done either by:
- A small chain-trigger enhancement to filter by output content, or
- Having the task always run and check the provider itself, returning early
  if it doesn't match.

The second is simpler and needs no engine changes.

**4. Naming convention for `SLACK_APP_CONFIG_TOKEN`.** The existing
auth-relay writes `<PROVIDER>_ACCESS_TOKEN` keyed on provider name. For
`slack-config`, that would be `SLACK_CONFIG_ACCESS_TOKEN`. Either accept
that name or add a small per-provider naming override in auth-relay. Not a
blocker.

### What the user does

```
$ dicode run slack-install-begin
OAuth flow started for slack-config.
Open this URL in a browser to authorize:
  https://relay.dicode.app/auth/slack-config?session_id=…&sig=…

[user clicks, approves apps.manifest:write scope]

[slack-install-complete chain-fires automatically]
Your personal dicode Slack app has been created.
Install it to your workspace:
  https://slack.com/oauth/v2/authorize?client_id=1234&scope=commands,chat:write&redirect_uri=…

[user clicks install link, approves in Slack]

[the normal Approach A manual step of copy-pasting tokens still happens here,
 UNLESS we wire the install link's redirect_uri to auth-relay as well —
 see Open Questions]
```

In the simplest form, Approach C automates steps 1–5 of Approach A's manual
setup (app creation, Request URL configuration, scope selection) but still
requires steps 6–7 (copying tokens into dicode secrets). With a small
additional wire-up — pointing the new app's OAuth redirect at the broker as
if it were an Approach B install — step 6 and 7 can also be automated.

### Missing pieces

- `slack-config` provider in broker [`providers.ts`](../../../dicode-relay/src/broker/providers.ts)
- Slack config-token OAuth client registered at api.slack.com by the dicode
  project
- `slack-install-begin` and `slack-install-complete` builtin tasks (~150
  lines of TS total)
- Manifest template
- Chain-trigger filtering-or-ignoring pattern (no engine changes if we
  check inside the task)
- Naming convention decision for the config token secret
- Documentation: one-page how-to for the `slack-install` flow

### Trade-offs

**Pros:**
- Best onboarding UX in the "user owns the Slack app" model.
- No new broker role — stays OAuth-only, no inbound event routing.
- Same install pattern generalizes to other platforms that have a
  manifest-create API (there aren't many, but Discord has something
  similar and GitHub Apps can be created this way too).
- Small, self-contained, no protocol changes.

**Cons:**
- Slack config tokens are a separate OAuth flow from bot-token OAuth,
  requiring a dedicated broker provider and a dedicated Slack OAuth client.
  Every additional provider is a small amount of setup friction on the
  dicode-project side.
- Still requires the user to click "Install to Workspace" on the generated
  app, meaning it's "one command + one more click," not truly one command.
- If Slack ever deprecates or rate-limits manifest creation, this approach
  breaks and users fall back to Approach A's fully manual flow.
- Produces one Slack app per user, same as Approach A — no marketplace
  presence, no shared distribution.

---

## Comparison table

| | **A: Manual** | **B: Shared app via broker** | **C: Manifest install task** |
|---|---|---|---|
| **Broker code changes** | none | ~200 lines | ~10 lines |
| **Daemon code changes** | none | ~50 lines | ~150 lines (tasks) |
| **Protocol bump** | no | yes (hello.bindings) | no |
| **Slack app owned by** | user | dicode project | user |
| **Slack app registrations at api.slack.com** | one per user | one total (by dicode project) | two total (by dicode project: config-token + each user's auto-created app) |
| **User steps to install** | 9 manual | 1 command + 1 click | 1 command + 2 clicks |
| **Blast radius of Slack-side issues** | one user | everyone | one user |
| **Inbound event routing** | direct via relay URL | broker lookup | direct via relay URL |
| **Self-hosted purity** | full | partial (shared broker app) | full |
| **Works today with zero changes?** | yes | no | no |

## Recommended rollout

The three approaches are **not mutually exclusive** — they share nothing at
the code level (Approach A has no code, B has inbound event routing, C has
builtin install tasks). A user installed under one approach is unaffected
by whether the others exist. This means you can pick any subset and stage
them independently.

**Phase 1 — Approach A.** Document the manual setup. Zero code. This is
the v1 target and a hard baseline: no matter what else ships, users can
always fall back to manual install.

**Phase 2 — Approach C.** Automates Approach A's manual clicks for users
who want a faster install without depending on a shared dicode Slack app.
Small, contained, reuses existing OAuth machinery. No broker architectural
shift.

**Phase 3 — Approach B.** Only if/when the project commits to a
marketplace-listed shared app and accepts the broker becoming an
inbound-events router. Largest code surface and the only one that requires
a protocol bump, but unlocks the best onboarding experience.

Nothing about this ordering is forced. Approach B could be built before C
if marketplace-listed UX is a priority. Approach C could be skipped entirely
if manual install is deemed good enough. The only approach that's
load-bearing is A, because it's the fallback when anything goes wrong with
the others.

## Open questions

1. **Can Approach C's manifest point at the broker instead of the user's
   relay URL?** If yes, Approach C becomes an installer for Approach B —
   creating a Slack app that routes through the shared broker endpoint,
   but owned per-user. This hybrid is interesting but probably
   unnecessary: if the broker already has the inbound routing, users
   would just use `auth-start provider=slack-bot` (the native Approach B
   install) rather than going through the manifest dance.

2. **Is Approach B's broker expansion worth it?** The jump from
   "OAuth-only" to "OAuth + event routing" is the biggest judgment call in
   this doc. If the project's long-term vision includes Discord, GitHub
   App webhooks, Linear webhooks, and so on — each one needing tenant
   routing — then building the pattern once and generalizing it is
   cheaper than maintaining Approach A + C forever. If Slack is expected
   to be the only chat integration, Approach B might never pay for
   itself.

3. **Should the `/slack/events` endpoint on the broker be the first
   instance of a more general "tenant-routed inbound webhook" abstraction?**
   Instead of a Slack-specific route, the broker could expose
   `POST /in/<provider>/events`, with a provider registry mapping
   `provider → (signature_verifier, tenant_extractor)`. Slack would be
   one entry; Discord, GitHub App, Linear could slot in later without new
   endpoints. More work upfront, cleaner foundation. Worth considering
   before committing to a Slack-specific endpoint in Phase 3.

4. **Config-token lifetime in Approach C.** Slack config tokens expire in
   12 hours. If the user starts an install but doesn't finish it
   within that window, the token expires and the chain task fails. We
   should surface a clear error and let them re-run `slack-install-begin`.
   Not a design problem, just an implementation detail to remember.

5. **Multiple Slack workspaces per dicode instance.** Approach B's
   handshake field is already an array (`slack_team_ids: string[]`), so
   the protocol can carry multiple team IDs. But Approach A and C
   assume a single `SLACK_BOT_TOKEN` / `SLACK_TEAM_ID` per dicode. To
   support "one user, two workspaces" under any approach, the secret
   naming convention would need to become per-team (e.g.
   `SLACK_BOT_TOKEN_T123`). Deferred; flag if it becomes a real request.

6. **Reserved `/hooks/slack-events` path.** Independent of approach
   choice: should the trigger engine refuse to let arbitrary user tasks
   bind this path, the way it reserves `/hooks/oauth-complete`? Probably
   yes, to prevent a random task from accidentally or deliberately
   intercepting Slack payloads. Small change to
   [`pkg/trigger/engine.go`](../../pkg/trigger/engine.go). Not a blocker for any approach but a good
   hardening step.
