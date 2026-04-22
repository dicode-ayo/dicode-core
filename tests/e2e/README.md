# e2e tests

Playwright suite covering the REST API, webhook triggers, cron, file-change
reconciliation, the SPA at `/hooks/webui`, and the auth flow. 60 tests,
~3.5 min end-to-end.

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
