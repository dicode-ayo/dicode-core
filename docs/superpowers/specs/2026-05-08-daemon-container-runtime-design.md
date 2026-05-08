# Daemon-Container Runtime Design

**Date:** 2026-05-08
**Status:** Draft — pending user review

## Summary

Extend dicode-core's task model to support a new "daemon container" execution mode: a long-lived container whose lifecycle is owned by dicode-core, and against which incoming webhooks dispatch either a `docker exec` command or an HTTP request, with the response returned to the caller. The same substrate serves three driving use cases — expensive container startup (model load), stateful sessions (browser, REPL), and long-running side processes (controlled via webhook).

This is distinct from existing daemon tasks (`runtime: deno|python` with `daemon: true`), which run a long-lived script in dicode's own process namespace. This design adds a parallel mechanism for containerized daemons whose interface to webhooks is not "stdout of one long-running script" but "per-request exec or HTTP into a persistent container".

## Goals

- Single declarative spec extension on docker-runtime tasks for daemon-container behavior.
- Two transport modes: `docker exec` for stateful exec-style invocation, and reverse-proxy HTTP for HTTP-shaped containers.
- Three lifecycle modes: `at-register`, `lazy`, `lazy-with-idle-ttl`.
- Configurable concurrency, ready/request timeouts, restart policy, and readiness probes.
- Security boundary: caller cannot control which path or argv reaches the container; HMAC verified at the trigger layer; container ports never bound to the host.
- Most logic driver-agnostic, so a podman implementation reuses ~70% of the code via a `ContainerHandle` interface.

## Non-Goals (v1)

- **Arbitrary-command exec from the webhook payload.** Argv is spec-pinned.
- **Cross-task container sharing via `target_container`.** Single-task → single-container in v1; named-network sharing is supported, but reusing one container across tasks is a follow-up.
- **Streaming responses to the webhook caller.** Sync request → fully buffered response.
- **Multi-replica daemon containers** (one image, N containers, load-balanced).
- **Podman driver.** v1 ships docker-only; podman is a follow-up landing on the same `ContainerHandle` interface.

## Spec Model

A new `daemon_container` block on a docker-runtime task:

```yaml
tasks:
  embed-server:
    runtime: docker
    image: my/embed:v1
    daemon_container:
      start: at-register              # at-register | lazy | lazy-with-idle-ttl
      idle_timeout: 10m               # only when start == lazy-with-idle-ttl
      ready_timeout: 30s              # webhook waits up to this for state==Ready
      max_concurrent: 0               # 0 = transport default (1 for exec, unbounded for http)
      network: auto                   # auto | <named-network> | none

      # Pick exactly one of `http` or `exec`. Transport is inferred from which is present.

      http:
        port: 8080                    # container port — internal-only, never published to host
        forward:
          path: /embed                # required; literal pin, NOT caller-controlled
        pass_through: true            # forward container status verbatim (vs. envelope-wrap)
        request_timeout: 60s          # duration of the forwarded HTTP call once ready
        readiness:
          # one of: http | log (omit → defaults to TCP port open)
          http: { path: /healthz, expect: 200 }
          # log: "Server listening on"
          initial_delay: 0s
          period: 1s
          timeout: 30s

      exec:
        command: ["/app/handle"]      # fixed argv; webhook params reach via env, body via stdin
        exec_timeout: 60s
        readiness:
          # one of: exec | log (omit → defaults to "container running")
          exec: ["test", "-f", "/tmp/ready"]
          # log: "Ready"
          initial_delay: 0s
          period: 1s
          timeout: 30s

    triggers:
      - webhook:
          path: /embed
          method: POST                # existing trigger-level method allowlist applies
    restart: on-failure               # reuses existing daemon restart policy
```

**Validation rules** (enforced at config load):

- Exactly one of `http` or `exec` is set.
- `http.port` required when `http` is set.
- `http.forward.path` required when `http` is set; must begin with `/`.
- `exec.command` required when `exec` is set; non-empty argv.
- `idle_timeout` only meaningful when `start == lazy-with-idle-ttl`; ignored otherwise.
- `network: none` forbids `http` transport (no IP routing possible).
- `runtime` must be `docker` (v1; `podman` added in follow-up without spec changes).

All existing docker-runtime config (volumes, env, secrets, image pull policy, resource limits) applies identically and is unchanged.

## Lifecycle State Machine

**States:**

| State | Meaning |
|---|---|
| Unstarted | Task registered with `start: lazy*`; no webhook arrived yet |
| Pulling | Image pull in progress (skipped if cached) |
| Starting | Container running, readiness probe not yet passing |
| Ready | Readiness probe passing; webhooks dispatch into the container |
| Degraded | Was Ready, probe now failing; transient retry window |
| CrashLooping | Repeated exit-during-Starting; webhooks fail fast |
| Stopping | `docker stop` in flight (SIGTERM → grace → SIGKILL) |
| Stopped | Container gone; restart policy decides what's next |

**Transitions:**

| From | Trigger | To |
|---|---|---|
| (registered) | `start: at-register` | Pulling → Starting |
| (registered) | `start: lazy` or `lazy-with-idle-ttl` | Unstarted |
| Unstarted | webhook arrives | Pulling → Starting |
| Starting | readiness probe passes | Ready |
| Starting | container exits | Stopped (then per restart policy) |
| Starting | exits ≥3× within 60s | CrashLooping |
| Ready | readiness probe fails | Degraded |
| Degraded | readiness probe passes | Ready |
| Degraded | persists past `ready_timeout` × 2 | Stopping → Stopped |
| Ready | container exits | Stopped (per restart policy) |
| Ready | idle ≥ `idle_timeout` (lazy-ttl only) | Stopping → Stopped (Unstarted) |
| any live state | task unregistered / dicode shutdown | Stopping → Stopped |
| CrashLooping | backoff window elapsed | Pulling → Starting |
| Stopped | restart policy says go | Pulling → Starting |

**Crash-loop detection:** ≥3 exits during `Starting` within 60s marks `CrashLooping`. Backoff doubles each cycle (10s → 20s → 40s, capped at 5min). Webhooks during backoff get HTTP 503 with `Retry-After`.

**Restart policy** (existing field, semantics unchanged):

- `always`: any exit → restart
- `on-failure`: exit ≠ 0 → restart; exit 0 → Stopped final
- `never`: any exit → Stopped final

**Webhook dispatch behavior by state:**

| State at webhook arrival | Behavior |
|---|---|
| Ready | Dispatch immediately |
| Unstarted (lazy) | Trigger Pulling, block up to `ready_timeout`, dispatch or 503 |
| Pulling, Starting, Degraded | Block up to `ready_timeout` for Ready, else 503 |
| CrashLooping | 503 immediately with `Retry-After` and crash-loop diagnostic |
| Stopping, Stopped (final) | 503 immediately |

## Transport Semantics

### HTTP transport — pure reverse proxy

Dicode acts as a thin reverse proxy. The caller crafts a normal HTTP request; dicode authenticates, rewrites the path, and forwards the rest verbatim. No dicode-specific contract leaks into the container.

**Request flow:**

1. Caller hits the dicode webhook trigger path (e.g. `POST /embed?model=v2` with JSON body); HMAC verified.
2. Dicode forwards to `http://<container-name>:<port><http.forward.path>` with:
   - **Method:** caller's method, passed through.
   - **Path:** replaced with the spec-declared `http.forward.path`. *Not caller-controlled.* Only path rewrite.
   - **Query string:** caller's verbatim.
   - **Headers:** caller's verbatim, minus dicode HMAC headers and hop-by-hop (`Connection`, `Transfer-Encoding`, `Upgrade`, etc.).
   - **Body:** verbatim.
3. Container's response: status + headers + body returned to caller verbatim. With `pass_through: false`, status is wrapped into a uniform envelope; with `true` (default), the container's status is the webhook's status.

**Timeouts:** two distinct knobs. `ready_timeout` for waiting on state == Ready; `request_timeout` for the actual forwarded HTTP call. Total caller-observed latency capped at the sum.

### exec transport — pinned argv with stdin/env

There is no equivalent "just proxy" mode; exec is shaped differently from HTTP. Convention:

1. Caller hits the dicode webhook trigger; HMAC verified; webhook params extracted (existing flow).
2. Dicode runs `docker exec <container> <exec.command>` with:
   - **argv:** spec-pinned `exec.command`, no caller-controlled substitution.
   - **stdin:** webhook request body piped verbatim.
   - **env:** webhook params injected as `DICODE_PARAM_<KEY>=value`.
3. **stdout** of the exec → webhook response body.
4. **exit 0** → HTTP 200. **exit ≠ 0** → HTTP 500 with envelope `{ "exit_code": N, "stdout": "...", "stderr": "..." }`.
5. `max_concurrent` exceeded → HTTP 429 with `Retry-After`.
6. `exec_timeout` exceeded → SIGTERM the exec'd process (container itself stays up); webhook gets HTTP 504.

### Symmetric guarantees

- **Path/argv is spec-declared, never caller-controlled.**
- **Body is verbatim** (HTTP body, exec stdin) — caller-supplied content reaches the container unmodified for that channel only.
- **Container HTTP port is never published to the host.** Reachable only by container name on the per-task or named docker network.
- **HMAC verified once at the dicode trigger boundary;** the container does not re-authenticate.

## Network Model

Configurable via `daemon_container.network`:

- **`auto` (default).** Per-task docker network `dicode-task-<id>` created at task register, destroyed at unregister. Container attached, dicode-core also attached, container reachable as `dicode-task-<id>`. One network per task; full lifecycle ownership.
- **Named network.** Dicode joins (or creates if absent) the named network. If dicode creates it, it's labeled `dicode.network=managed` for orphan reaping but is **not deleted on unregister** — other consumers may need it. This is the multi-container case: declare two daemon-container tasks with `network: my-app-net`, run a sidecar postgres yourself on `my-app-net`, and all peers see each other by container name. Same semantics as docker-compose external networks.
- **`none`.** Container runs network-isolated. Exec-only transport works; HTTP transport rejected at validation.

**Dicode-core reachability of the network** depends on deployment:

- Dicode-core in docker → attaches itself to each per-task or named network at create time, via the docker socket.
- Dicode-core on host → uses container IP from the docker bridge, looked up via `docker inspect` and cached. Fallback path; in-docker is the recommended deployment.

## Architecture & Code Layout

```
pkg/trigger/engine.go
   ├── on task-register: if DaemonContainer != nil, instantiate manager
   ├── containerManagers map[taskID]*Manager
   └── webhook handler routes to manager.Dispatch() before falling through to one-shot path

pkg/runtime/daemoncontainer/   ← NEW package, driver-agnostic
   ├── manager.go              FSM, mutex, max_concurrent semaphore, ready-wait channel,
   │                           idle timer, crash-loop detector, restart backoff
   ├── readiness.go            probe types: http / log / exec / tcp-open / docker-healthcheck
   ├── transport_http.go       net/http/httputil.ReverseProxy with HMAC strip,
   │                           path pin, hop-by-hop strip, pass_through toggle
   ├── transport_exec.go       docker exec wrapper: pinned argv, stdin = body,
   │                           env = params, captures stdout/stderr/exit
   └── state.go                state machine types

pkg/runtime/executor.go        leave existing Executor interface alone; add:
                               ContainerHandle interface beside it

pkg/runtime/docker/
   ├── docker.go               existing one-shot path, unchanged
   └── daemon.go               NEW: ContainerHandle impl using Docker Go SDK,
                               per-task network create/attach/destroy, label-based reap

pkg/task/spec.go               add DaemonContainer struct with HTTPTransport,
                               ExecTransport, ReadinessProbe sub-structs;
                               validation in (s *Spec).Validate()

pkg/runtime/podman/            (deferred follow-up)
   └── daemon.go               ContainerHandle impl shelling out to podman CLI
```

### `ContainerHandle` interface (new abstraction)

```go
type ContainerHandle interface {
    Start(ctx context.Context) error
    Exec(ctx context.Context, argv []string, stdin io.Reader, env map[string]string) (ExecResult, error)
    HTTPRoundTripper() http.RoundTripper
    Stop(ctx context.Context, grace time.Duration) error
    WaitExit() <-chan ExitInfo
    NetworkAttach(name string) error
}
```

The seam between driver-agnostic logic (`pkg/runtime/daemoncontainer/`) and driver-specific code (`pkg/runtime/docker/daemon.go`, future `pkg/runtime/podman/daemon.go`).

### Concurrency primitives in `manager.go`

- One `sync.RWMutex` guarding state field and ready-wait channel.
- One semaphore (buffered chan) sized to `max_concurrent` gating dispatch (1 for exec by default, unbounded for HTTP).
- Idle timer reset on every dispatch (lazy-with-idle-ttl mode only).

### Persistence on dicode restart

v1 has no persistence. On dicode boot:

1. Reap orphan containers labeled `dicode.task-id` (existing pattern).
2. Reap orphan networks labeled `dicode.network=managed` whose `dicode.task-id` no longer exists in current spec.
3. Re-create from spec — daemon-container managers initialize fresh, run their `at-register` startups, etc.

State machine state is in-memory only. Webhook callers see 503 during the boot window until containers reach Ready.

## Failure Semantics (caller-observable)

| Case | HTTP status | Body |
|---|---|---|
| Container ready, dispatch successful | 200 (or container's status if HTTP `pass_through: true`) | Container's response / exec stdout |
| Cold-start in progress, exceeded `ready_timeout` | 503 | `Retry-After` + state diagnostic |
| Crash-looping | 503 | `Retry-After` (remaining backoff) + crash-loop diagnostic |
| Container died mid-call | 502 | "container disappeared mid-call" diagnostic |
| Exec returned non-zero | 500 | `{exit_code, stdout, stderr}` envelope |
| HTTP container returned 5xx, `pass_through: true` | 5xx (verbatim) | Container's body verbatim |
| HTTP container returned 5xx, `pass_through: false` | 502 | Envelope with original status & body |
| `max_concurrent` exceeded | 429 | `Retry-After` |
| Stopping / Stopped final (restart: never) | 503 | "task disabled" diagnostic |
| `request_timeout` / `exec_timeout` exceeded | 504 | "upstream timeout" diagnostic |

## Observability

**Per-task structured logs** at every state transition: `task_id`, prev state, new state, reason, restart count, backoff remaining (if applicable), exit code (if from container exit).

**Metrics** (Prometheus, reusing existing dicode metric machinery):

- `dc_container_state{task,state}` — gauge, 0/1 per state per task
- `dc_ready_wait_seconds{task}` — histogram of webhook → Ready wait time
- `dc_request_duration_seconds{task,transport,outcome}` — histogram
- `dc_exec_duration_seconds{task}` — histogram (exec transport only)
- `dc_crashloop_total{task}` — counter
- `dc_idle_evictions_total{task}` — counter
- `dc_concurrent_pressure{task}` — gauge of in-flight dispatches

## Testing Strategy

**Unit tests** (`pkg/runtime/daemoncontainer/`, no docker required):

- FSM transitions for every (state, event) pair
- Readiness probe types: HTTP probe (mock server), log probe (regex matching), TCP-open probe (net.Listen), exec probe (fake `ContainerHandle.Exec`)
- HTTP transport: header strip, path pin, hop-by-hop strip, `pass_through` modes (against `httptest.Server` + a fake `ContainerHandle.HTTPRoundTripper`)
- Exec transport: stdin/env/argv pinning, exit code propagation, timeout cancellation
- Concurrency: `max_concurrent` semaphore enforcement; ready-wait channel correctness under racing webhooks
- Crash-loop detector: timing-windowed counter, backoff escalation

**Integration tests** (real docker, in CI):

- Tiny test images: `alpine` + `python -m http.server` for HTTP transport; `alpine` + `sleep infinity` + simple shell scripts for exec
- All `start` modes (`at-register`, `lazy`, `lazy-with-idle-ttl` with short TTL)
- All probe types end-to-end
- Crash induction (exit 1 in init) → CrashLooping, then fix → Ready
- Idle TTL eviction → next webhook restarts container
- Restart policy permutations (`always` / `on-failure` / `never`) crossed with normal exit and crash exit
- Network modes: `auto`, named (with a peer container in the same network), `none` (exec-only)

**E2E tests:**

- Existing trigger-engine test patterns extended: declare a daemon-container task in test config, hit the webhook, assert response shape
- Multi-task scenario on a shared named network: two daemon-container tasks plus a sidecar verifying inter-container DNS works

## Podman Inheritance

Most of the design is driver-agnostic by deliberate construction.

**Free for podman** (everything in `pkg/runtime/daemoncontainer/`):

- Whole FSM (states, transitions, crash-loop detection, restart backoff)
- Per-manager mutex and `max_concurrent` semaphore
- Idle TTL timer
- HTTP transport (reverse proxy is just `http.RoundTripper`)
- All readiness probe types — expressed against `ContainerHandle`, no driver-specific code
- All metrics & logs

**Needs podman-specific implementation** (`pkg/runtime/podman/daemon.go`):

- `ContainerHandle` impl mapping `Start`/`Exec`/`Stop`/`WaitExit` to `podman` CLI calls (consistent with current podman runtime style)
- Network create/attach/detach via `podman network ...`
- `HEALTHCHECK` parsing via `podman inspect`
- **Risk: rootless podman networking.** Slirp4netns has different semantics for "internal-only" ports. Needs verification that internal-network-only routing works between peer containers without host port binding. If broken, ship rootful first with documented caveat.

**Estimate:** ~1 week if rootless behaves; ~2 weeks if rootless edge cases force compromise. Separate PR after docker lands.

## Open Questions Deferred to Implementation

- **Log handling for daemon containers.** A long-lived container produces an unbounded log stream; existing daemon-task patterns (deno/python) already wrestle with this. v1 adopts whatever those tasks do today and revisits if rotation/retention is inadequate.
- **Image pull policy for daemon containers.** Reuses the existing docker-runtime pull policy field; daemon-container behavior pulls at lifecycle entry to `Pulling` state.
- **Secrets injection.** Reuses existing docker-runtime secrets/env mechanisms; surfaces in the container at start, not per-call.

## Migration & Rollout

- **No migration burden** for existing tasks — `daemon_container` is a new optional block on docker-runtime tasks.
- **Backward-compatible.** Existing one-shot docker tasks continue to work; existing daemon-script tasks (deno/python) continue to work; this is purely additive.
- **Feature flag** unnecessary; the feature is opt-in via the spec field. Tasks without `daemon_container` follow the existing one-shot path with no code change.

## Effort Estimate

- Driver-agnostic manager + transports + readiness + state machine: ~1 week
- Docker `ContainerHandle` impl + network management + label reaping: ~1 week
- Trigger-engine integration + spec validation: ~3 days
- Tests (unit + integration + E2E): ~1 week
- Documentation: ~2 days
- **Total v1 (docker-only): ~3 weeks**
- Podman follow-up: +1–2 weeks
