# e2e tests

Playwright suite covering the REST API, webhook triggers, cron, file-change
reconciliation, the SPA at `/hooks/webui`, the auth flow, and the MCP
JSON-RPC surface. 70 tests, ~3.5 min end-to-end.

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

`playwright.config.ts` defines three projects:

| Project | Server config | What runs | storageState |
|---|---|---|---|
| `unauthenticated` | `auth: false`, no passphrase | webhooks, cron, file-change, config specs | seeded session |
| `webui` | same as above | `webui-task.spec.ts` — SPA tests | seeded session |
| `authenticated` | `auth: true`, `secret: test-passphrase-12345` | `auth.spec.ts` only | none (tests the login flow) |

The authenticated project is a separate server start, hence a separate
`make` target.

## Test inventory

70 tests total: 43 in `unauthenticated` + 14 in `webui` + 13 in `authenticated`.

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

### [file-change.spec.ts](file-change.spec.ts) — File Change Detection (4 tests)

Mutates the per-run temp copy of fixtures and verifies the reconciler picks
changes up via fsnotify.

| # | Test | Verifies |
|---|---|---|
| 1 | editing task.js updates task behaviour | New `console.log` appears in a fresh run's logs after rewrite. |
| 2 | editing task.yaml (description) is reflected in API response | `/api/tasks/{id}.description` updates within 20 s. |
| 3 | UI reflects file changes after reconciler picks them up | `dc-task-detail` still resolves after a task script rewrite. |
| 4 | file edit is idempotent — restoring original brings task back | Undoing the edit restores the original description. |

### [mcp.spec.ts](mcp.spec.ts) — MCP JSON-RPC surface (10 tests)

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

### [config.spec.ts](config.spec.ts) — Config API + UI (9 tests)

#### Config API (6 tests)

| # | Test | Verifies |
|---|---|---|
| 1 | GET /api/config returns config object with our test port | `server.port` = 8765 round-trips. |
| 2 | GET /api/config does not leak secret field | `server.secret` is absent under any casing (`json:"-"` respected). |
| 3 | GET /api/config returns sources array including e2e-tests | `sources[]` contains the `e2e-tests` local source. |
| 4 | GET /api/config/raw returns YAML content | Raw-YAML endpoint returns the live `dicode.yaml` contents. |
| 5 | POST /api/config/raw rejects invalid YAML with 400 | Bad YAML is refused before persistence. |
| 6 | POST /api/config/raw persists valid YAML and round-trips a marker | Writes + reads a comment marker, then restores the original. |

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

## Fixtures

[fixtures/tasks/](fixtures/tasks/) — four test tasks exercising each trigger
type (manual, cron, open webhook, HMAC webhook) plus the buildin `webui` task
referenced by path so the SPA loads.

[fixtures/dicode-unauth.yaml](fixtures/dicode-unauth.yaml) /
[dicode-auth.yaml](fixtures/dicode-auth.yaml) — server configs with
`execution.max_concurrent_tasks: 2` to bound concurrent Deno subprocesses.

## Port

The suite binds `localhost:8765` — must be free. Override with
`DICODE_URL=http://localhost:<port>` plus matching edits to the fixture
`server.port` values if you need a different port.
