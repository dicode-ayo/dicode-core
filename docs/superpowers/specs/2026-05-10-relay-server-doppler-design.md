# Relay-server buildin + Doppler-fed config (ESO-style)

**Date:** 2026-05-10
**Status:** Design — ready for implementation
**Authors:** dicode-ayo

## Goal

Run `dicode-relay` on a Raspberry Pi (or any single host) supervised by dicode-core, with all OAuth client secrets and the status password sourced from a Doppler workspace via the existing `buildin/secret-providers/doppler` task. Make `dicode-core` act as the "external secrets operator" equivalent for the relay: controller fetches from Doppler, injects values into the consumer at start time, supervises the process lifecycle.

## Non-goals

- Kubernetes deployment. The design targets a single-host install (Pi, VM, laptop).
- Hot reloading on Doppler rotation. Rotation is pull-on-restart (`dicode tasks restart buildin/relay-server`) plus the doppler buildin's existing 5-minute cache.
- A generic `secret-template` reusable buildin. That is a clean follow-up but requires new core API (`dicode.secrets_resolve` IPC method + a new permission). Tracked separately.
- Public-URL / NAT-traversal solution. Documented as a deployment prerequisite (Cloudflare Tunnel is the recommended path); not solved in code.
- Multiple relay instances on the same host.

## Architecture

One new dicode-core buildin: `tasks/buildin/relay-server/`. Daemon-style task (`trigger.daemon: true`, `restart: always`) modeled on `buildin/relay-client`. The task runs the relay **in-process under Deno** — no subprocess, no shell-out.

ESO mental model:

| ESO concept              | dicode equivalent                                 |
|--------------------------|---------------------------------------------------|
| Secrets backend          | Doppler API                                       |
| `SecretStore` controller | `buildin/secret-providers/doppler` (existing)     |
| Reconcile loop           | daemon env-resolver + provider task cache (5m)    |
| `ExternalSecret` consumer| `buildin/relay-server` task (new)                 |
| Materialized Secret      | in-memory env injection (no rendered file)        |
| Workload                 | `dicode-relay` running inside the task's Deno process |

### Why no rendered file

Approach was considered (write `relay.yaml` to `$DICODE_DATADIR/relay/relay.yaml` with secrets substituted, relay reads it). Rejected:

- Plaintext secrets on disk for no operational benefit — the relay never re-reads its config at runtime.
- Two artifacts to keep in sync (the template + the rendered output).
- The relay's existing `${VAR}` interpolation already turns env into config; we get the same effect with zero new code.

### Why in-process (not subprocess)

User constraint: no shell-out from inside a Deno task. The relay was already designed to run as a CLI bin (`bin: dicode-relay`), but a small refactor exposes a programmatic entry point (`startServer`) that the Deno task can `await` directly. Node-compat under Deno is already proven by `buildin/auth-relay`, which imports `npm:dicode-relay/client`.

## Components

### Files in `dicode-core` (new)

```
tasks/buildin/relay-server/
  task.yaml      # daemon spec; permissions.env; params.base_url
  task.ts        # Deno supervisor body
  relay.yaml     # shipped config template with ${VAR} placeholders
  task.test.ts   # Deno tests using startServer({ dryRun: true })
```

Plus one entry added to `tasks/buildin/taskset.yaml` registering `relay-server` in the buildin source.

### Files in `dicode-relay` (changed/new)

```
src/start.ts          # NEW — exported startServer(opts) (extracted from index.ts)
src/index.ts          # REWRITTEN — thin CLI bin (~25 lines: parse --check/--config → call startServer → SIGTERM wiring)
package.json          # exports: { ".", "./start", "./config" }; version 0.1.5 → 0.2.0
tests/start.test.ts   # NEW — dryRun-based unit coverage + one real listen smoke
tests/index.test.ts   # extended — covers the new --check flag
```

The existing CLI behavior is preserved end-to-end (the bin still works for non-dicode-core users running the relay standalone).

## `startServer` API

```ts
// src/start.ts (exported as npm:dicode-relay@^0.2/start)
export interface StartOpts {
  configPath?: string;             // path to relay.yaml; mutually exclusive with `config`
  config?: RelayConfig;            // pre-loaded config object; bypasses loadConfig
  env?: NodeJS.ProcessEnv;         // env source for ${VAR} interpolation (default: process.env)
  dryRun?: boolean;                // validate + wire up but don't listen()
}

export interface StartHandle {
  httpServer: http.Server | https.Server;  // listening === false when dryRun: true
  relayServer: RelayServer;
  close(): Promise<void>;
}

export async function startServer(opts?: StartOpts): Promise<StartHandle>;
```

When `dryRun: true`:
- Run `loadConfig` (env interpolation + Zod validation).
- Run `loadBrokerSigningKey()` (so disk/permission issues surface).
- Call `buildProviderMap`, `buildGrantMiddleware`, `buildBrokerRouter` (so config-shape errors surface).
- **Skip** `httpServer.listen()`; start no timers, no WS handshake.
- Return a handle whose `close()` releases in-memory state and is a no-op against the unbound socket.

CLI surface: `dicode-relay --check` forwards to `startServer({ dryRun: true })`, exits 0 on success, non-zero with diagnostic on failure.

## `buildin/relay-server` task spec

```yaml
# tasks/buildin/relay-server/task.yaml
apiVersion: dicode/v1
kind: Task
name: "Relay Server"
description: >
  Runs dicode-relay in-process under the daemon's Deno runtime. OAuth client
  secrets and the status password come from Doppler via the existing
  buildin/secret-providers/doppler task. Set base_url via task overrides:
    dicode tasks override buildin/relay-server --param base_url=https://...

runtime: deno

trigger:
  daemon: true
  restart: always

params:
  base_url:
    type: string
    required: true
    description: >
      Public URL the relay is reachable at (used as the OAuth callback origin).
      Example: https://relay.example.com (or your Cloudflare Tunnel hostname).

permissions:
  net:
    - "*"
  env:
    - DICODE_DATADIR
    - DICODE_VERSION
    - name: STATUS_PASSWORD
      from: task:secret-providers/doppler
    # OAuth providers — all optional. Relay skips any provider with empty client_id.
    - name: GITHUB_CLIENT_ID
      from: task:secret-providers/doppler
      optional: true
    - name: GITHUB_CLIENT_SECRET
      from: task:secret-providers/doppler
      optional: true
    - name: GOOGLE_CLIENT_ID
      from: task:secret-providers/doppler
      optional: true
    - name: GOOGLE_CLIENT_SECRET
      from: task:secret-providers/doppler
      optional: true
    # ... (full list: see relay.yaml.example for provider set; all optional)

notify:
  on_success: false
  on_failure: true
```

```ts
// tasks/buildin/relay-server/task.ts (sketch)
import { startServer } from "npm:dicode-relay@^0.2/start";
import type { DicodeSdk } from "../../sdk.ts";

export default async function main({ params }: DicodeSdk) {
  const baseUrl = await params.get("base_url");
  if (!baseUrl) throw new Error("base_url param is required");

  // Relay's loadConfig reads ${VAR} from process.env; surface base_url as BASE_URL.
  Deno.env.set("BASE_URL", baseUrl);

  const configPath = new URL("./relay.yaml", import.meta.url).pathname;
  const handle = await startServer({ configPath });

  const shutdown = async () => {
    try { await handle.close(); } finally { Deno.exit(0); }
  };
  Deno.addSignalListener("SIGTERM", shutdown);
  Deno.addSignalListener("SIGINT", shutdown);

  // Keep the task alive while the relay is running.
  await new Promise<void>((resolve) => {
    handle.httpServer.on("close", () => resolve());
  });
}
```

## `relay.yaml` shipped template

Lives at `tasks/buildin/relay-server/relay.yaml`. Same structure as `relay.yaml.example`, with two pinned values for dicode-core integration:

```yaml
server:
  port: 5553
  base_url: ${BASE_URL}     # task param → env → interpolation
  tls:
    cert_file: ""
    key_file: ""

status:
  password: ${STATUS_PASSWORD}

broker:
  session_ttl_ms: 300000
  # Persist across task restarts so existing sessions survive.
  signing_key_file: ${DICODE_DATADIR}/relay/broker-signing.key

  providers:
    # ... same provider list as relay.yaml.example, all ${VAR}-based
```

## Bootstrap flow on the Pi

Prerequisites:
1. dicode-core installed (existing onboarding flow; Deno runtime included).
2. A public URL reaching port 5553 (recommended: Cloudflare Tunnel; out of scope here).

Doppler side (one-time):
3. Doppler project + config populated with `STATUS_PASSWORD` and `<PROVIDER>_CLIENT_ID` / `<PROVIDER>_CLIENT_SECRET` per provider you want enabled.
4. Generate a read-only Doppler service token scoped to that config.

On the Pi:
5. Seed the token: `dicode secrets set DOPPLER_TOKEN dp.st.xxxxxxxx`.
6. Set the base URL: `dicode tasks override buildin/relay-server --param base_url=https://relay.example.com` (or via the WebUI overrides dialog).
7. Daemon reconciler enables and starts `buildin/relay-server` automatically.

Verify:
8. `curl http://localhost:5553/health` → `{"ok":true}`.
9. `curl http://localhost:5553/status -u admin:$STATUS_PASSWORD` → dashboard.
10. Hit the public URL from outside.

Rotation:
- Rotate a secret in Doppler → next task restart picks it up (`dicode tasks restart buildin/relay-server`). The provider's 5m cache also auto-expires on its next dispatch.

## Data flow (cold start)

1. dicode-core daemon reconciler: `buildin/relay-server` is enabled, daemon-style, `restart: always`.
2. Daemon resolves `permissions.env`: each `from: task:secret-providers/doppler` entry triggers one dispatch of the doppler provider with the union of requested keys. Provider hits `api.doppler.com`, returns values, cached 5m.
3. Daemon resolves `params.base_url` from `Overrides.Params["base_url"]`. Required-with-no-default → task fails to start if unset.
4. Daemon spawns the Deno task with env = `{ <resolved secrets>, BASE_URL=<param>, DICODE_DATADIR=..., DICODE_VERSION=... }`.
5. Task body imports `startServer` from `npm:dicode-relay@^0.2/start`, computes the path to `./relay.yaml`, calls `await startServer({ configPath })`.
6. Relay's `loadConfig` reads the template, walks it, substitutes `${VAR}` from `process.env` (which Deno's node-compat populated from the task env). Validates against Zod.
7. Relay builds express app, binds port, starts WebSocket server + metrics + Grant/broker routes. Returns handle.
8. Task body installs SIGTERM/SIGINT handlers and awaits the server's `close` event.

OAuth flow itself is unchanged (Grant → broker → encrypted token push to `auth-relay` via WS tunnel).

## Failure modes

The task is a normal daemon-style buildin; existing core machinery (crash-loop detection, `notify.on_failure`, dashboard status, override-driven recovery) handles every failure surface. No relay-specific error plumbing. Specifically:

- Doppler unreachable or `DOPPLER_TOKEN` unset → provider task fails → relay-server fails fast → crash-loop backoff.
- Required secret missing in Doppler → provider throws `required_secret_missing` → task fails with the key named in the log.
- `base_url` param unset → daemon refuses to dispatch; dashboard surfaces "missing required param".
- Port already bound, signing key unwritable, malformed `relay.yaml` → `startServer` rejects → task exits → restart cycle.
- `npm:dicode-relay@^0.2/start` fetch fails on first cold start when offline → documented bootstrap prereq (outbound HTTPS on first start, cached after).

## Testing

### dicode-relay side (Vitest, Node)

- `tests/start.test.ts`:
  - `dryRun: true` cases: minimal valid config, malformed YAML, missing required `${VAR}`, unwritable signing-key path. No ports involved.
  - `dryRun: false` × 1 (smoke): real listen, hit `/health`, call `close()`, assert port released.
- `tests/index.test.ts`: existing CLI smoke + new `--check` exits 0/non-zero correctly.

### dicode-core side

- `tasks/buildin/relay-server/task.test.ts` (Deno test): calls into the task body's logic — reads `base_url` param, sets env, invokes `startServer({ configPath, dryRun: true })` against the shipped `relay.yaml`. Asserts:
  - With valid fixture: resolves cleanly.
  - With `base_url` param missing: throws.
  - With required secret missing: throws.
- Go-side: existing reconciler tests pick up the new buildin once registered in `tasks/buildin/taskset.yaml`.

Integration on a real Pi against real Doppler is a manual verification step, not CI.

## Cross-repo coordination

- `dicode-relay` PR lands first and publishes `0.2.0` to npm.
- `dicode-core` PR pins `npm:dicode-relay@^0.2/start` in `tasks/buildin/relay-server/task.ts`. Can be developed in parallel against a local link or pre-release tag, but merge order is: relay → core.

## Open questions

- Whether to ship a CLI helper (`dicode relay set-base-url <url>`) as syntactic sugar for the `tasks override` invocation. Defer until users ask.
- Whether the relay's `loadBrokerSigningKey` default location should change in 0.2.0 (currently auto-generates next to CWD when path is empty). For dicode-core use we always set the path explicitly via the template, so no change required.

## Tracked work

- dicode-relay issue: `startServer` library extraction + `dryRun` + 0.2.0 release.
- dicode-core issue: `buildin/relay-server` task + template + tests.
