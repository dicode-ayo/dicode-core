# e2e tests

Playwright suite covering the REST API, webhook triggers, cron, file-change
reconciliation, the SPA at `/hooks/webui`, the auth flow, auth-provider
connections, dev-mode/clone-mode, task suspend/resume, run-input persistence,
and the MCP JSON-RPC surface. 128 tests, ~3.5 min end-to-end (136 with the
opt-in relay project).

## One-time setup

```bash
make test-e2e-install   # npm install && npx playwright install chromium
```

## Running

```bash
make test-e2e           # full suite: unauthenticated + webui + authenticated
make test-e2e-unauth    # unauthenticated + webui projects only (~3 min)
make test-e2e-auth      # authenticated project only (~20 s)
```

Override the HMAC test secret with `E2E_WEBHOOK_SECRET=...`.

### Running a single spec or test

```bash
# single spec
npx playwright test webhooks.spec.ts

# single test by title substring
npx playwright test -g "webhook run navigable in UI"

# specific project
npx playwright test --project=webui
DICODE_AUTH_MODE=authenticated npx playwright test --project=authenticated
```

## Seeing what the browser is doing

**When you have a display** (local dev, VS Code desktop, X/Wayland forwarded into
the container):

```bash
make test-e2e-headed    # visible Chromium window
make test-e2e-ui        # Playwright UI mode — live rerun, watch, step-through
```

Or drop `await page.pause()` in a spec and run with `PWDEBUG=1` to stop at
that line in the Inspector:

```bash
PWDEBUG=1 npx playwright test webui-task.spec.ts
```

**When you have no display** (plain SSH, headless devcontainer):

```bash
npx playwright show-report
```

Hosts the HTML report on `http://localhost:9323` with screenshots for every
failure. VS Code's builtin port-forwarding sends it to your local browser.

Or open a specific trace — timeline with DOM snapshot, console, and network
tab at every step:

```bash
npx playwright show-trace test-results/<test-dir>/trace.zip
```

Traces and screenshots are captured **only on failure** by default
([playwright.config.ts](../../playwright.config.ts)). To capture them for
passing runs too, temporarily flip `screenshot`/`trace` to `'on'`.

## How the suite boots dicode

[helpers/dicode-server.ts](helpers/dicode-server.ts) is the Playwright global
setup:

1. Rebuilds the `dicode` binary if any Go source is newer.
2. Creates a fresh temp dir per run, copies [fixtures/tasks/](fixtures/tasks/)
   into it, and substitutes placeholders (`FIXTURES_TASKS_DIR`,
   `BUILDIN_WEBUI_TASK_YAML`, `TEMP_DATA_DIR`, `TEMP_TASKSET_PATH`) to get a
   concrete `dicode.yaml` + `taskset.yaml`.
3. Spawns `dicode daemon --config ...` on port 8765 with:
   - `GOMEMLIMIT=512MiB` — soft heap ceiling
   - `DICODE_DISABLE_UNLOCK_LIMITER=1` — lets the auth suite hammer
     `/api/auth/login` past the normal 5-per-minute cap
4. Waits for `/api/tasks` to come up.
5. POSTs to `/api/auth/login` to seed a session cookie and writes it to
   `tests/e2e/.auth-state.json` — Playwright's `storageState` for the
   `unauthenticated` and `webui` projects (the `/hooks/webui` task has
   `trigger.auth: true` so even unauth-mode browser navigation needs a
   cookie).

Teardown kills the daemon, removes the temp dir, and deletes the auth-state
file.

## Projects

`playwright.config.ts` defines four projects:

| Project | Server config | What runs | storageState |
|---|---|---|---|
| `unauthenticated` | `auth: false`, no passphrase | webhooks, webhooks-secure, cron, file-change, approval-review, pending-task-list-signals, config, mcp, dev-mode-clone, run-input-persistence, task-toggle, suspend-resume, cli-suspend specs | seeded session |
| `webui` | same as above | `webui-task.spec.ts` — SPA tests | seeded session |
| `authenticated` | `auth: true`, `secret: test-passphrase-12345` | `auth.spec.ts`, `auth-providers.spec.ts` | none (tests the login flow) |
| `relay` | separate broker + daemon pair on random ports, not the shared global-setup daemon | `relay-protocol.spec.ts`, `relay-buildin.spec.ts` — opt-in via `DICODE_E2E_RELAY=1` | none |

The authenticated project is a separate server start, hence a separate
`make` target.

## Test inventory

128 non-relay tests total: 91 in `unauthenticated` + 14 in `webui` + 23 in
`authenticated` (136 including the 8 opt-in `relay` tests).

### [webhooks.spec.ts](webhooks.spec.ts) — Open Webhook (8 tests)

Covers `/hooks/test-webhook` (no HMAC). Fires the `hello-webhook` task,
verifies response shape and log persistence.

| # | Test | Verifies |
|---|---|---|
| 1 | POST to webhook returns 200 with JSON body | Basic synchronous fire returns 200. |
| 2 | POST sets X-Run-Id response header | Response carries `X-Run-Id` for run correlation. |
| 3 | webhook run result contains input payload | Task return `{received: input}` echoes the POST body. |
| 4 | POST with ?wait=false returns runId immediately | Async mode returns runId without waiting for completion. |
| 5 | run triggered by webhook appears in /api/runs | Run is queryable via `GET /api/runs/{id}` with correct `task_id`. |
| 6 | webhook run navigable in UI | `dc-run-detail` renders at `/hooks/webui/runs/{id}` with a `.badge-success`. |
| 7 | GET to webhook without index.html fires task | GET falls through to task execution (not 404). |
| 8 | webhook logs contain received input | `/api/runs/{id}/logs` contains the task's stdout ("webhook received …"). |

### [webhooks-secure.spec.ts](webhooks-secure.spec.ts) — HMAC Webhook (7 tests)

Covers `/hooks/test-webhook-secure` with an HMAC-SHA256 secret
(`webhook_secret: ${TEST_WEBHOOK_SECRET}` in the fixture).

| # | Test | Verifies |
|---|---|---|
| 1 | POST without signature header → 403 | Missing `X-Hub-Signature-256` is rejected. |
| 2 | POST with wrong signature → 403 | Wrong digest is rejected. |
| 3 | POST with correct signature → 200 | Valid signature fires the task. |
| 4 | signed request sets X-Run-Id header | Valid signed fire still emits the correlation header. |
| 5 | signed webhook run completes successfully | Async run reaches `success` status within 30 s. |
| 6 | signed request result contains input payload | Task return echoes input on authenticated fires. |
| 7 | signature on wrong body → 403 | Signature computed over a different body is rejected (tamper detection). |

### [cron.spec.ts](cron.spec.ts) — Cron Tasks (5 tests)

Covers the `hello-cron` task (`* * * * *`). Spec-level timeout 120 s to
accommodate the 90 s wait.

| # | Test | Verifies |
|---|---|---|
| 1 | cron task fires at least once within 90 seconds | `/api/tasks/{id}/runs` populates without manual trigger. |
| 2 | cron run status is success | The first completed cron run has `Status: success`. |
| 3 | cron run has logs | Run logs contain the expected `"cron tick"` stdout. |
| 4 | task list shows last run status after cron fires | `dc-task-list` row for `hello-cron` gets a status `.badge`. |
| 5 | cron task detail shows trigger label with cron expression | `dc-task-detail` trigger card text matches `/cron|every minute|\*/i`. |

### [file-change.spec.ts](file-change.spec.ts) — File Change Detection (5 tests)

Mutates the per-run temp copy of fixtures and verifies the reconciler picks
changes up via fsnotify.

| # | Test | Verifies |
|---|---|---|
| 1 | editing task.js updates task behaviour | New `console.log` appears in a fresh run's logs after rewrite. |
| 2 | editing task.yaml (description) is reflected in API response | `/api/tasks/{id}.description` updates within 20 s. |
| 3 | UI reflects file changes after reconciler picks them up | `dc-task-detail` still resolves after a task script rewrite. |
| 4 | fsnotify pickup latency is within budget (< 1500 ms) | Reconciler picks up an edit fast enough for the UI's "instant reload" claim. |
| 5 | file edit is idempotent — restoring original brings task back | Undoing the edit restores the original description. |

### [mcp.spec.ts](mcp.spec.ts) — MCP JSON-RPC surface (11 tests)

Covers the buildin/mcp dicode task served at `/mcp` (via the API-key-gated
forwarder in pkg/webui that re-dispatches to `/hooks/mcp`).

| # | Test | Verifies |
|---|---|---|
| 1 | GET /mcp returns server-info JSON | Legacy probe compat with the old pkg/mcp Go server. |
| 2 | initialize returns capabilities + protocolVersion | MCP `initialize` round-trip with `2024-11-05`. |
| 3 | tools/list returns the six expected tool definitions | Surface parity with the old Go server. |
| 4 | tools/call list_tasks returns dicode task list | The buildin task list is reachable through the SDK. |
| 5 | tools/call get_task returns the spec for a known task | Single-task lookup via `dicode.list_tasks` + filter. |
| 6 | tools/call get_task with unknown id returns -32603 | Errors are returned in JSON-RPC envelopes, not HTTP errors. |
| 7 | tools/call list_sources returns a hint | Tools without SDK access surface a /api/sources hint. |
| 8 | unknown method returns -32601 | Method-not-found path. |
| 9 | tools/call with unknown tool name returns -32603 | Tool-not-found path. |
| 10 | empty body returns parse error -32700 with id:null | Parse-error response shape per JSON-RPC 2.0. |
| 11 | direct POST /hooks/mcp is rejected (no session) | Only the API-key forwarder at `/mcp` may reach the buildin task; direct webhook posts are session-gated. |

### [config.spec.ts](config.spec.ts) — Config API + UI (12 tests)

#### Config API (9 tests)

| # | Test | Verifies |
|---|---|---|
| 1 | GET /api/config returns config object with our test port | `server.port` = 8765 round-trips. |
| 2 | GET /api/config does not leak secret field | `server.secret` is absent under any casing (`json:"-"` respected). |
| 3 | GET /api/config exposes spec.entries including e2e-tests | `spec.entries` contains the `e2e-tests` source in the current TaskSet shape. |
| 4 | GET /api/sources lists e2e-tests as a taskset source | `/api/sources` surfaces the same source by name. |
| 5 | POST /api/settings/sources rejects a git:// url with 400 | The `git://` scheme (no auth support) is rejected at validation. |
| 6 | GET /api/settings/sources/git/branches (preview) rejects a git:// url with 400 | Same scheme guard on the branch-preview endpoint. |
| 7 | GET /api/config/raw returns YAML content | Raw-YAML endpoint returns the live `dicode.yaml` contents. |
| 8 | POST /api/config/raw rejects invalid YAML with 400 | Bad YAML is refused before persistence. |
| 9 | POST /api/config/raw persists valid YAML and round-trips a marker | Writes + reads a comment marker, then restores the original. |

#### Config UI (3 tests)

| # | Test | Verifies |
|---|---|---|
| 1 | navigating to /config shows config page | `dc-config` renders on `/hooks/webui/config`. |
| 2 | config page contains server settings section | `dc-config` text includes server/config keywords. |
| 3 | header nav link navigates to config page | Clicking the header `Config` link routes to `/hooks/webui/config`. |

### [webui-task.spec.ts](webui-task.spec.ts) — SPA at /hooks/webui (14 tests)

Runs in the `webui` Playwright project. Uses the seeded session cookie.

#### Task List (6 tests)

| # | Test | Verifies |
|---|---|---|
| 1 | dashboard loads at /hooks/webui | Initial navigation lands under the webui base with `dc-task-list`. |
| 2 | renders Tasks heading | `<h1>Tasks</h1>` is visible. |
| 3 | task list has header columns | Thead contains ID / Name / Trigger / Last Run / Status. |
| 4 | shows tasks from registered task sets | At least one `<tbody tr>` row appears. |
| 5 | tasks are grouped by namespace when namespaces exist | Namespace label spans render above each task group. |
| 6 | clicking a task row navigates to task detail | `<td a>` click pushes `/hooks/webui/tasks/…` and mounts `dc-task-detail`. |

#### Task Detail (3 tests)

| # | Test | Verifies |
|---|---|---|
| 1 | task detail page shows task name | `<h1>` renders the task name after loading finishes. |
| 2 | task detail shows Run now button | `Run now` button is visible. |
| 3 | task detail shows recent runs section | `<h2>Recent runs</h2>` is present. |

#### Run Detail (2 tests)

| # | Test | Verifies |
|---|---|---|
| 1 | triggering a run navigates to run detail | `Run now` → POST `/api/tasks/{id}/run` → URL → `/hooks/webui/runs/{id}` → `.badge` visible. |
| 2 | run detail page shows Logs heading | `<h2>Logs</h2>` and `#log-output` render for an API-fired run. |

#### Navigation (3 tests)

| # | Test | Verifies |
|---|---|---|
| 1 | nav link to Sources navigates client-side | Header `Sources` link → `/hooks/webui/sources` + `dc-sources`. |
| 2 | nav link to Config navigates client-side | Header `Config` link → `/hooks/webui/config` + `dc-config`. |
| 3 | nav link Tasks returns to task list | After navigating away, header `Tasks` link returns to `dc-task-list`. |

### [auth.spec.ts](auth.spec.ts) — Authentication (13 tests)

Runs in the `authenticated` project (server started with `auth: true` and
`secret: test-passphrase-12345`). No pre-seeded session.

| # | Test | Verifies |
|---|---|---|
| 1 | unauthenticated GET /api/tasks → 401 | API is locked down without a session. |
| 2 | unauthenticated GET /api/runs/{id} → 401 | Same for run endpoints. |
| 3 | POST /api/auth/login with wrong passphrase → 401 | Wrong passphrase is rejected. |
| 4 | POST /api/auth/login with correct passphrase → 200 | Correct passphrase returns `{status: "ok"}`. |
| 5 | session cookie is set after successful login | Response `Set-Cookie` contains `dicode_secrets_sess=`. |
| 6 | authenticated request to /api/tasks succeeds | After login, API reads return 200. |
| 7 | webhooks bypass auth wall (no session required) | `POST /hooks/test-webhook` succeeds without a cookie. |
| 8 | UI: GET /hooks/webui without session redirects to /login with next | 303 to `/login?next=/hooks/webui`. |
| 9 | UI: /login renders an HTML form with a password input | Login page has a password field. |
| 10 | UI: submitting /login form with wrong passphrase shows error | Body contains `/[Ii]ncorrect|[Ii]nvalid|[Ww]rong/`. |
| 11 | UI: submitting /login form with correct passphrase loads SPA | Form submit redirects to `/hooks/webui` and `dc-task-list` renders. |
| 12 | GET /api/auth/passphrase status returns source after login | `source: "yaml"` (fixture sets `server.secret` in YAML). |
| 13 | POST /api/auth/logout invalidates session | Post-logout, `/api/tasks` returns 401 again. |

### [auth-providers.spec.ts](auth-providers.spec.ts) — Auth Providers dashboard (10 tests)

Runs in the `authenticated` project. Covers the buildin/auth-providers task:
listing configured OAuth providers and driving `connect` for both
relay-broker and standalone (BYO token) flows.

| # | Test | Verifies |
|---|---|---|
| 1 | GET /hooks/auth-providers serves index.html with SDK injected | Dashboard page loads for a logged-in session. |
| 2 | list action returns provider rows with has_token false by default | No secret configured → `has_token: false`. |
| 3 | list returns the post-#256 PublicProviderInfo shape | Response matches the current provider-info schema. |
| 4 | list reports has_token=true when an ACCESS_TOKEN secret is set | Presence of the configured secret flips `has_token`. |
| 5 | list returns standalones only when relay is disabled (BYO without relay) | Broker-backed providers are hidden when `relay.enabled` is false. |
| 6 | connect with broker provider when relay is disabled returns a useful 5xx | Clear error instead of a silent failure when relay is off. |
| 7 | list auto-discovers _oauth-app inheritors via the template marker | Tasks inheriting the `_oauth-app` template are surfaced automatically. |
| 8 | connect routes _oauth-app inheritors to their webhook | Connect action resolves to the inheriting task's webhook. |
| 9 | connect with standalone openrouter returns the webhook URL | BYO-token provider returns a webhook URL to POST the token to. |
| 10 | connect with unknown provider returns 5xx | Unknown provider id fails loudly rather than silently. |

### [dev-mode-clone.spec.ts](dev-mode-clone.spec.ts) — Dev-mode clone + on_failure_chain (22 tests)

Runs in the `unauthenticated` project. Covers the dev-mode-with-branch and
`on_failure_chain` features from #236/#241: local-path dev mode, clone-mode
(requires a git-type source, skipped otherwise), `run_id`/branch validation,
the MCP `switch_dev_mode` tool schema, and both the bare-string and
structured forms of `on_failure_chain`.

| # | Test | Verifies |
|---|---|---|
| 1 | PATCH local_path → 200 with local_path in body | Local-path dev mode is unaffected by the clone-mode addition. |
| 2 | clone-mode enable returns 200 with branch/run_id in body (git source only) | Enabling clone-mode against a git source returns the resolved branch and run id. |
| 3 | run_id validation rejects malformed ids → 400 (parameterized: 6 cases via `invalidRunIDs`) | `ValidateRunID` rejects disallowed characters/shapes. |
| 4 | branch validation rejects malformed branch names → 400 (parameterized: 7 cases via `invalidBranchNames`) | `ValidateBranchName` rejects disallowed characters/shapes. |
| 5 | empty branch → 200 (local-path/disable path, not clone-mode) | An empty branch string takes the local-path/disable code path rather than erroring. |
| 6 | second concurrent clone-mode enable → 400 dev-mode busy (git source only) | Concurrency guard prevents two clone-mode enables racing. |
| 7 | tools/list: switch_dev_mode schema includes branch, base, run_id | MCP tool schema advertises the new clone-mode params. |
| 8 | tools/call switch_dev_mode round-trips branch/base/run_id in hint text | MCP tool call surfaces the params it was given back in its response. |
| 9 | loop-target fixture (bare-string on_failure_chain) is registered without error | Legacy bare-string `on_failure_chain: task-id` still parses at startup. |
| 10 | will-fail fires chain-target with user params + reserved keys in input | Structured-form `on_failure_chain` merges user params with reserved chain metadata. |
| 11 | chain-fired run that fails does not trigger its own on_failure_chain | Chain-triggered runs don't recursively re-chain on failure. |

### [run-input-persistence.spec.ts](run-input-persistence.spec.ts) — Run-input persistence (7 tests)

Runs in the `unauthenticated` project. Covers the run-input persistence
pipeline from #233: encrypted-at-rest storage of sensitive webhook
input, redaction, and retention-driven cleanup.

| # | Test | Verifies |
|---|---|---|
| 1 | encrypted blob is written to ${DATADIR}/run-inputs/<runID>.bin | A sensitive webhook POST results in an on-disk encrypted blob. |
| 2 | plaintext sensitive values are absent from the on-disk blob | The blob is genuinely encrypted, not just relocated. |
| 3 | GET /api/runs/<runID> returns InputStorageKey, InputSize>0, InputStoredAt>0, InputPinned=0 | Run metadata reflects the persisted input correctly. |
| 4 | InputRedactedFields lists all expected sensitive dotted paths | Redaction covers every configured sensitive field path. |
| 5 | e2e-tests/run-inputs-cleanup runs to completion without error | The cleanup task itself is registered and runnable. |
| 6 | persisted blob is NOT deleted by cleanup (default 30-day retention is far in the future) | Default retention doesn't prematurely delete recent blobs. |
| 7 | SKIP: asserting deletion requires dicode.yaml defaults.run_inputs.retention: 0s | Placeholder — the e2e harness doesn't yet parameterize retention, so actual deletion isn't exercised. |

### [task-toggle.spec.ts](task-toggle.spec.ts) — Task enable/disable toggle (5 tests)

Runs in the `unauthenticated` project. Covers `PATCH /api/tasks/{id}/overrides`
and the corresponding WebUI toggle.

| # | Test | Verifies |
|---|---|---|
| 1 | PATCH /api/tasks/{id}/overrides sets enabled=false | Override API disables a task. |
| 2 | PATCH unknown task returns 404 | Unknown task id is rejected. |
| 3 | PATCH unknown field returns 400 | Unsupported override field is rejected. |
| 4 | UI toggle flips the row and persists across reload | Task-list toggle control reflects and survives a page reload. |
| 5 | dc-toast surfaces a visible message when the API rejects a toggle | UI shows an error toast on a failed toggle. |

### [pending-task-list-signals.spec.ts](pending-task-list-signals.spec.ts) — Pending-approval signals on the task list (4 tests)

Runs in the `unauthenticated` project. Covers #650: a pending (unapproved)
task must not present as live and healthy in the task list.

| # | Test | Verifies |
|---|---|---|
| 1 | a pending row reads as held, not live: no green dot, no dead link, Run disabled | The toggle drops the plain "on" green tint and gets a pending-specific tooltip; the trigger column doesn't hyperlink a route that 404s; Run is disabled with an explanatory tooltip. |
| 2 | a pending count/filter appears on the list and narrows it | A "N pending approval" chip appears and, when clicked, narrows the list to only pending tasks. |
| 3 | the notification tray surfaces the pending transition | The existing `approval:pending` WebSocket event now reaches `dc-notif-panel`, producing a visible, reviewable inbox entry. |
| 4 | a Run failure surfaces via toast, never a native alert | Any `/run` failure (not just the pending-approval case) renders as a `dc-toast`, not a browser `alert()`. |

### [suspend-resume.spec.ts](suspend-resume.spec.ts) — Suspend/resume WebUI (2 tests)

Runs in the `unauthenticated` project. Covers the suspend/resume WebUI
surface from #512: a task calling `dicode.suspend()`, the resulting
`suspended` run with a JSON Schema, submitting the form via
`POST /api/runs/<id>/resume`, and the continuation run.

| # | Test | Verifies |
|---|---|---|
| 1 | suspend → fill form → resume spawns the continuation | Full round-trip: suspend, submit input, continuation run succeeds with the submitted value. |
| 2 | a suspended run's result page redirects to the resume form | Visiting a suspended run's result page routes to the resume form instead of a normal run-detail view. |

### [cli-suspend.spec.ts](cli-suspend.spec.ts) — CLI suspend/resume (3 tests)

Runs in the `unauthenticated` project. Drives the real `dicode` binary
(not just the WebUI) against a suspending task, non-TTY only.

| # | Test | Verifies |
|---|---|---|
| 1 | run --non-interactive prints the suspended run id and exits | `dicode run --non-interactive` doesn't hang on a suspending task; prints the run id and exits. |
| 2 | run --field auto-advances the wizard to success | Pre-supplied `--field` answers let a suspending task's wizard auto-advance to completion. |
| 3 | resume with no args lists the suspended run | Bare `dicode resume` lists suspended runs rather than erroring. |

## Fixtures

[fixtures/tasks/](fixtures/tasks/) — four test tasks exercising each trigger
type (manual, cron, open webhook, HMAC webhook) plus the buildin `webui` task
referenced by path so the SPA loads.

[fixtures/dicode-unauth.yaml](fixtures/dicode-unauth.yaml) /
[dicode-auth.yaml](fixtures/dicode-auth.yaml) — server configs with
`execution.max_concurrent_tasks: 2` to bound concurrent Deno subprocesses.

## Relay project

The `relay` Playwright project exercises the buildin/relay-client task and
buildin/auth-relay task end-to-end against a real dicode-relay broker
subprocess. Unlike the other projects it does **not** share the global-setup
daemon — it spawns its own broker + daemon pair on random ports and tears them
down after the suite finishes.

Skipped by default; opt in via:

```bash
DICODE_E2E_RELAY=1 npx playwright test --project=relay
```

Requires `dicode-relay` as a dev dependency (already in `package.json` after
`npm install`).

Two specs cover the relay-client + auth-relay integration from different
angles:

| Spec | Relay source | Purpose |
|---|---|---|
| [relay-protocol.spec.ts](relay-protocol.spec.ts) | Separate `node dicode-relay` subprocess | Fastest protocol-level signal: independent broker startup, no daemon task supervision involved |
| [relay-buildin.spec.ts](relay-buildin.spec.ts) | `buildin/relay-server-body` runs inside the daemon | Production-path coverage: relay broker is a Deno daemon task supervised by the trigger engine |

`relay-buildin.spec.ts` pre-writes `${DATADIR}/relay/relay.yaml` with an
ephemeral port, then uses `spec.entries` overrides in the daemon config to
inject `DICODE_E2E_MOCK_PROVIDER=1` into the relay-server-body task subprocess
so `/_test/deliver` is available.

### What the relay suite tests

| # | Test | Verifies |
|---|---|---|
| 1 | relay-client connects and publishes status | `GET /api/relay/status` returns `connected:true` and a valid `hook_base_url`. |
| 2 | identity UUID is stable across daemon restart | Daemon killed + respawned; same 64-hex UUID appears in the re-connected status. |
| 3 | forward path: broker URL reaches daemon webui | `GET http://broker/u/<uuid>/api/tasks` is forwarded through the WSS tunnel and returns JSON. |
| 4 | auth flow: mock broker delivery writes secret | `POST /_test/deliver` pushes a synthetic ECIES envelope through the tunnel; broker returns non-404, proving the relay-client is registered. |

The auth-flow test uses the broker's built-in e2e mock (`DICODE_E2E_MOCK_PROVIDER=1`). It confirms the end-to-end delivery path (broker → WSS → daemon → auth-relay) without requiring a real OAuth provider.

## Port

The suite binds `localhost:8765` — must be free. Override with
`DICODE_URL=http://localhost:<port>` plus matching edits to the fixture
`server.port` values if you need a different port.
