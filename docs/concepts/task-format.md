# Task Format

Every dicode task is a folder containing up to three files:

```text
tasks/
└── morning-email-check/
    ├── task.yaml       ← required: trigger, params, permissions, metadata
    ├── task.ts         ← required: TypeScript/JS logic (Deno runtime)
    └── task.test.ts    ← optional: unit tests
```

When using a TaskSet source, the folder name is not the task ID — instead, the ID is built from the namespace path (e.g. `infra/morning-email-check`).

---

## `task.yaml`

All task files must declare `apiVersion` and `kind`:

### Minimal example

```yaml
apiVersion: dicode/v1
kind: Task
name: Morning Email Check
trigger:
  cron: "0 8 * * *"
```

### Full example

```yaml
apiVersion: dicode/v1
kind: Task
name: Morning Email Digest
description: Fetches unread emails and posts a summary to Slack
runtime: deno

trigger:
  cron: "0 8 * * 1-5"   # weekdays at 8am

params:
  slack_channel:
    description: Slack channel to post digest
    default: "#general"
  max_emails:
    description: Maximum emails to include
    default: "20"

permissions:
  env:
    - GMAIL_TOKEN
    - SLACK_TOKEN
```

### All fields

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | ✅ | Human-readable task name |
| `description` | string | | One-line description |
| `runtime` | string | | `deno` (default), `python`, `docker`, or `podman` |
| `trigger` | object | ✅ | Exactly one trigger must be set |
| `trigger.cron` | string | | Standard cron expression (5 fields) |
| `trigger.webhook` | string | | Webhook path, e.g. `/github-push` |
| `trigger.auth` | bool | | Require a valid dicode session for webhook GET (UI) and POST (run) |
| `trigger.manual` | bool | | Set `true` to enable manual-only |
| `trigger.chain` | object | | Chain trigger (see below) |
| `trigger.chain.from` | string | | Task ID to listen for |
| `trigger.chain.on` | string | | `success` (default), `failure`, `always` |
| `trigger.chain.params` | map | | User-defined keys merged into the downstream `input` map alongside engine-reserved keys (`taskID`, `runID`, `status`, `output`, `_chain_depth`). When omitted, `input` is the upstream's raw output unchanged. See [chain params and per-edge overrides](#chain-params-and-per-edge-overrides). |
| `trigger.chain.overrides` | object | | Per-edge patch applied to a deep copy of the downstream's spec at firing time; manual fires of the same downstream are unaffected. See [per-edge overrides](#chain-params-and-per-edge-overrides). |
| `trigger.daemon` | bool | | Start on app start, restart on exit |
| `trigger.restart` | string | | daemon only: `always` (default), `on-failure`, `never` |
| `permissions` | object | | Explicit access grants — nothing is implicit |
| `permissions.env` | list | | Env vars the script may read (see below); for unrestricted env read use `env_read_exposed` |
| `permissions.env_read_exposed` | bool | | Grant unrestricted env-var reads (Deno and Python): Deno gets bare `--allow-env`; Python lifts the os.environ filter added in #418. For node-compat / npm tasks (see below) |
| `permissions.fs` | list | | Filesystem access declarations (Deno: read+write; Python: write only) |
| `permissions.fs[].path` | string | | Absolute or `~`-prefixed path |
| `permissions.fs[].permission` | string | | `r`, `w`, or `rw` |
| `permissions.run` | list of strings | | Executables the script may spawn (Deno and Python); use `["*"]` for all |
| `permissions.net` | list of strings | | Outbound network hosts. `["*"]` = unrestricted. Omit or `[]` = deny all (Deno/Python: per-host enforcement; Docker/Podman: `network_mode: none` unless `docker.ports` are published — in which case networking stays enabled for port binding). Specific hosts are enforced per-host for Deno/Python; for Docker/Podman they are allowed but not yet per-host enforced (a warning is logged). |
| `permissions.sys` | list of strings | | Deno sys APIs (Deno only); omit = deny all, `["*"]` = all |
| `permissions.dicode` | object | | Which dicode runtime APIs the task may call (all denied by default) |
| `permissions.dicode.tasks` | list of strings | | Task IDs the script may invoke via `dicode.run_task()`; use `["*"]` for all |
| `permissions.dicode.mcp` | list of strings | | MCP daemon task IDs the script may call via `mcp.call()`; use `["*"]` for all |
| `permissions.dicode.list_tasks` | bool | | Allow `dicode.list_tasks()` |
| `permissions.dicode.get_runs` | bool | | Allow `dicode.get_runs()` |
| `permissions.dicode.secrets_write` | bool | | Allow `dicode.secrets_set()` and `dicode.secrets_delete()` — write-only, no read |
| `permissions.dicode.secrets_has` | bool | | Allow `dicode.secrets.has(key)` — boolean presence check, never returns the value |
| `permissions.dicode.crypto` | list of strings | | Context names allowed for `dicode.crypto.encrypt/decrypt`; `["*"]` for all user-accessible contexts. Daemon-private contexts (e.g. `dicode/run-inputs/v1`) are never accessible to tasks. |
| `params` | list | | Input parameters with defaults |
| `params[].name` | string | | Parameter name |
| `params[].description` | string | | Human-readable description |
| `params[].default` | string | | Default value (all params are strings) |
| `tags` | list of strings | | Tags for filtering (future: source selectors) |
| `mcp_exposed` | bool | | When `false` (default), the task is hidden from MCP `tools/list` and `tools/call`. Set to `true` to expose the task to MCP clients. |
| `run_result` | object | | Per-task return-value persistence config — see [Suppressing return-value persistence](#suppressing-return-value-persistence) |
| `run_result.enabled` | bool | | When `false`, the JSON return value is not written to `runs.return_value`; in-memory delivery (`dicode.run_task`, chain `input.output`) is unaffected. Default `true`. |

### Trigger types

**Cron** — runs on a schedule:

```yaml
trigger:
  cron: "*/15 * * * *"   # every 15 minutes
```

Uses standard 5-field cron syntax. Evaluated against the machine's local timezone.

**Webhook** — fires on HTTP POST:

```yaml
trigger:
  webhook: /github-push
```

Endpoint: `POST /hooks/github-push`. Request body available as `input` global in `task.js`.

To require a valid dicode session before allowing access to the webhook UI or running the task, add `auth: true`:

```yaml
trigger:
  webhook: /hooks/my-internal-tool
  auth: true
```

- `GET /hooks/my-internal-tool` (serving `index.html`) → redirects to `/?auth=required` if no session
- `POST /hooks/my-internal-tool` (running the task) → returns `401` JSON if no session
- `dicode.js` handles 401 automatically: silent refresh via device token, then redirects to login
- Open webhooks (no `auth: true`) remain fully public — no behaviour change

`auth` accepts three values:

- `true` / `"session"` — a valid dicode session is required (as above). If a `webhook_secret` is also set, a session **and** a valid HMAC signature are both required.
- `"any"` — a valid session **or** a valid HMAC signature authenticates. Requires a `webhook_secret`. Because HMAC is the only credential that traverses the [relay](../webhooks.md), this is the way to let a **signed machine caller** authenticate over the public relay URL while a browser still uses its session directly. A plain browser (no signature) still can't authenticate over the relay — that stays a tunnel's job. `GET`/UI-asset requests always require a session, never a signature.
- absent / `false` — public.

```yaml
trigger:
  webhook: /hooks/my-machine-endpoint
  auth: any
  webhook_secret: "${MY_WEBHOOK_SECRET}"
```

**Manual** — only fires when explicitly triggered via API or UI:

```yaml
trigger:
  manual: true
```

**Daemon** — starts automatically when dicode starts and restarts when it exits.

```yaml
trigger:
  daemon: true
  restart: always   # always (default) | on-failure | never
```

- **`always`** (default) — restarts whenever the task exits (success, failure). Does not restart if explicitly killed.
- **`on-failure`** — only restarts on non-zero exit / script error. Stops if the task succeeds.
- **`never`** — starts once on app start, never restarts.

**Stale run detection:** if dicode is killed without a clean shutdown, any "running" runs from the previous session are automatically marked "cancelled" on the next startup, so the history stays accurate and daemon tasks start fresh.

**Graceful shutdown:** when dicode stops, all daemon tasks receive a kill signal (SIGTERM for Docker tasks, context cancellation for JS tasks) before the process exits.

A 2-second back-off is applied between restarts to prevent tight loops on immediately-failing tasks.

**Chain** — fires when another task completes:

```yaml
trigger:
  chain:
    from: fetch-emails
    on: success    # success | failure | always
```

The completing task's return value is available as the `input` global.

### Chain params and per-edge overrides

By default, `input` in the downstream task is the upstream's raw return
value (a string, map, whatever the upstream returned). Two optional
fields let a chain edge enrich or specialize that dispatch without
modifying the upstream or making the downstream daemon-aware.

**`trigger.chain.params`** — operator-defined keys merged into the
downstream's `input` map alongside engine-reserved keys. The upstream's
return value lands under `input.output`; the rest of the user-defined
keys appear at top level.

```yaml
# task-b/task.yaml — fires when task-a succeeds
trigger:
  chain:
    from: task-a
    params:
      destination: "#alerts"
      verbose: true
```

Inside `task-b`, the shape is:

```typescript
input.destination   // "#alerts"
input.verbose       // true
input.output        // task-a's return value
input.taskID        // "task-a"  (engine-reserved)
input.runID         // upstream run ID
input.status        // "success"
input._chain_depth  // 0 on success chains
```

Reserved keys (`taskID`, `runID`, `status`, `output`, `_chain_depth`)
are rejected at config-load if present in `params`. When `params` is
empty (the default), `input` stays as the upstream's raw value — no
wrapping — so existing chains keep working unchanged.

String values in `params` may reference the upstream's runtime state
via the dispatch-time interpolation grammar — `${input.output}`,
`${input.output.<field>}`, `${input.params.<name>}`, and embedded
forms. See [`${input.…}` interpolation](#input-interpolation) below
for the full grammar and loud-failure semantics.

The same params shape applies symmetrically to
`on_failure_chain.params` (the failure-chain analogue).

**`trigger.chain.overrides`** — a per-firing patch applied to a deep
copy of the downstream's spec right before dispatch via this edge.
Manual / cron / direct-API fires of the same downstream task are
unaffected.

```yaml
trigger:
  chain:
    from: task-a
    overrides:
      timeout: 5m
      env:
        - name: MODE
          value: chain
```

Per-edge overrides accept a conservative subset of the global
taskset-entry override surface:

| Field | Allowed at per-edge site? |
| --- | --- |
| `params`, `env`, `net`, `fs`, `timeout`, `notify`, `dicode`, `runtime` | yes |
| `trigger`, `enabled`, `name`, `description`, `retry`, `defaults`, `entries` | no (rejected at load) |

The `trigger` rejection is deliberate — per-edge dispatch invokes the
downstream directly and ignores any rewired trigger config on the merged
spec, so silently accepting it would mislead operators.

<a id="input-interpolation"></a>
**`${input.…}` interpolation.** Three reference shapes are recognised
at dispatch time in `trigger.chain.params` values (and, equivalently,
in `kind: PipelineTask` stage overrides):

| Form | Resolves to | Loud-fail when |
| --- | --- | --- |
| `${input.output}` | upstream's full string return value | upstream returned a non-string, or the string is empty |
| `${input.output.<field>}` | named string field of an object-shaped upstream return (e.g. `{path: "..."}`) | upstream isn't an object, field absent, field non-string, or the field's string value is empty |
| `${input.params.<name>}` | named entry from the upstream's `RunOptions.Params` (the caller-supplied params on the upstream's fire) | params map nil, named entry missing, or the entry's value is empty |

Empty strings are treated as "not provided" uniformly: a token whose
resolution would yield `""` fails loudly with `ErrInputUnavailable`,
identifying the offending param and token — values are never
substituted silently. Embedded forms
(`"prefix-${input.output}-suffix"`) and multi-token forms
(`"${input.params.scheme}://${input.output.host}"`) are supported.
Unknown shapes (e.g. `${input.foo}`, `${input.params}` with no field)
are rejected at task-registration time with a site-qualified error.

The `<field>` / `<name>` portion is a permissive identifier: the first
character must be a letter or underscore; the remainder may include
letters, digits, underscores, hyphens, and dots — fitting shapes like
`${input.params.x-forwarded-for}` and `${input.output.db.host}`.

```yaml
trigger:
  chain:
    from: render-config
    params:
      content: "${input.output}"          # bare token
      destPath: "${input.output.path}"    # named field
      endpoint: "${input.params.url}"     # piped caller param
      banner: "rendered at ${input.output} on ${input.params.host}"
```

## Pipelines

A **pipeline** is a `kind: PipelineTask` that declares an ordered list
of stages. Each stage is an existing `kind: Task`, run as its own child
run. A stage must reach `status=success` for the pipeline to advance;
the first failure short-circuits the rest. The previous stage's return
value is piped forward via `${input.output}`, so a render → persist →
start-daemon composition lives in one self-contained pipeline
definition.

A pipeline lives in a task folder like any other task — the file is
still **`task.yaml`**, discriminated by its `kind:`. There is no
separate filename; the loader reads `kind: PipelineTask` and parses the
pipeline schema instead of the `kind: Task` schema.

```yaml
apiVersion: dicode/v1
kind: PipelineTask
name: Render and Serve
description: Render config, persist it, then run the daemon body.
subtype: sequential

trigger:
  manual: true        # how the PIPELINE is fired (optional)

stages:
  - task: buildin/template       # stage 1: render
    overrides:
      params:
        - name: template_path
          default: "${TASK_DIR}/config.tmpl"
  - task: buildin/write-local    # stage 2: persist stage 1's output
    overrides:
      params:
        - name: content
          default: "${input.output}"
        - name: path
          default: "${DATADIR}/app/config.yml"
      fs:
        - path: "${DATADIR}/app"
          permission: rw
  - task: app-daemon-body        # terminal stage: a kind: Task daemon
```

### Pipeline fields

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `apiVersion` | string | ✅ | Must be `dicode/v1` |
| `kind` | string | ✅ | Must be `PipelineTask` |
| `name` | string | ✅ | Human-readable pipeline name |
| `description` | string | | One-line (or multi-line) description |
| `subtype` | string | ✅ | `sequential` (stages run in declaration order) or `parallel` (stages with no `depends_on` run concurrently; stages with `depends_on: [stage-id]` wait for all listed dependencies). Error semantics for parallel: fail-fast (first failure cancels in-flight siblings). |
| `trigger` | object | | How the **pipeline** is fired. At most one trigger type. Omit for a pipeline that's only fired programmatically (e.g. via `dicode.run_task`). |
| `trigger.manual` | bool | | Manual-only fire (API / UI) |
| `trigger.cron` | string | | Standard 5-field cron expression |
| `trigger.webhook` | string | | Webhook path, e.g. `/deploy` |
| `trigger.webhook_secret` | string | | HMAC secret for the webhook (supports `${VAR}` expansion) |
| `trigger.auth` | bool | | Require a valid dicode session for the webhook (same semantics as `kind: Task`) |
| `trigger.chain` | object | | Chain trigger — fire the pipeline when an upstream task/pipeline completes (see [chain](#chain-params-and-per-edge-overrides)) |
| `stages` | list | ✅ | Ordered list of stages; at least one required |
| `stages[].id` | string | | Stage identifier, used as a `depends_on` target in `subtype: parallel` pipelines |
| `stages[].task` | string | ✅ | Task ID of an existing `kind: Task` to run as this stage |
| `stages[].depends_on` | []string | | List of stage IDs this stage depends on. Only valid for `subtype: parallel`. All dependencies must complete before this stage runs. |
| `stages[].overrides` | object | | Per-stage patch applied to the stage task's spec at dispatch time (see [stage overrides](#stage-overrides)) |
| `timeout` | duration | | Overall pipeline timeout (e.g. `5m`). Cancels the in-flight stage and fails the pipeline if exceeded. |

> **No `trigger.daemon` on a pipeline.** A pipeline does not have a
> daemon trigger. It becomes daemon-shaped *implicitly* when its
> **terminal stage** is a `kind: Task` with `trigger.daemon: true` —
> see [Daemon terminal stage](#daemon-terminal-stage) below.

### Sequential semantics

`subtype: sequential` runs the stages in declaration order:

1. Each non-terminal stage is fired as a child run and the pipeline
   **waits for it to reach `success`** before advancing.
2. The stage's return value is threaded into the next stage as
   `${input.output}` (see [stage input threading](#stage-input-threading)).
3. The first stage to reach `failure` / `timeout` / `cancelled`
   **short-circuits** the pipeline: the remaining stages are never
   fired, the pipeline's run goes to `failure`, and `fail_reason` is
   set to `stage N (<task-id>): <error>` (N is **0-based**, so the
   first stage failing reports `stage 0 (...)`).

### Stage input threading

Stages share the same `${input.…}` interpolation grammar as
[chain params](#chain-params-and-per-edge-overrides), evaluated at dispatch
time against the **previous stage's** output:

| Form | Resolves to |
| --- | --- |
| `${input.output}` | the previous stage's full string return value |
| `${input.output.<field>}` | a named string field of an object-shaped previous-stage return |

These tokens are resolved **only** inside `stages[].overrides.params`
defaults. Other override fields (`env[].value`, `fs[].path`, `timeout`,
…) are applied verbatim — a `${input.…}` token there is not interpolated
and would be passed through literally. The same loud-failure rules apply:
a token in a param default that would resolve to an empty string fails
the stage with `ErrInputUnavailable` rather than substituting silently.

**v1 threads `Output` only.** `${input.params.<name>}` (referencing the
*upstream stage's* input params) is **not** supported in sequential
pipelines and is rejected at load time on every stage. Cross-stage param
threading is a planned follow-up; for now, thread data forward through
each stage's **return value** (`${input.output}`).

**The first stage receives no input.** Any `${input.…}` reference in
`stages[0].overrides` is rejected at load time — there is no upstream
stage to resolve it against.

### Stage overrides

A stage may carry an `overrides:` block that patches a deep copy of the
stage task's spec at dispatch time — the underlying `kind: Task` is
untouched, so a manual / cron / chain fire of that same task is
unaffected. Stage overrides accept a slightly **wider** allowlist than
[`trigger.chain.overrides`](#chain-params-and-per-edge-overrides):
crucially, a stage **may override the stage task's `trigger`**.

| Field | Allowed at a pipeline stage? |
| --- | --- |
| `params`, `env`, `net`, `fs`, `timeout`, `dicode`, `runtime` | yes |
| `trigger` | **yes** (unlike chain edges) — e.g. flip the terminal stage's `daemon`-ness |
| `enabled`, `name`, `description`, `retry`, `defaults`, `entries` | no (rejected at load) |

A stage runs **regardless of the stage task's own trigger type**: the
engine dispatches the merged stage spec directly and never gates on
whether the underlying `kind: Task` is `manual` / `cron` / etc. So a
`manual: true` library task fires as a stage with **no** trigger
override — for example the shipped `buildin/relay-server` pipeline fires
`buildin/write-local` (which is `trigger.manual: true`) as a stage and
overrides only `params` / `fs` / `timeout`, never `trigger`.

The `trigger` override is allowed at a stage so a stage can flip
**daemon-ness** — set or clear `daemon: true` on the merged spec to
make (or unmake) the terminal stage a daemon (see
[Daemon terminal stage](#daemon-terminal-stage)). This is exactly why a
per-edge `trigger` override is *rejected* on `trigger.chain` edges but
permitted at a pipeline stage:

```yaml
stages:
  - task: my-server-task
    overrides:
      trigger:
        daemon: true    # make this the pipeline's terminal daemon stage
```

`${input.…}` references resolve at dispatch time, so they are stripped
during the registration-time merge-validation and resolved fresh on
each fire.

### Daemon terminal stage

If the **last** stage is a `kind: Task` with `trigger.daemon: true`, the
pipeline is *daemon-shaped*: its lifetime is tied to the daemon's run.

- The render/persist stages run to `success` as usual, then the daemon
  stage is fired **without** a wait-to-success gate.
- The pipeline's run stays **`running`** for as long as the daemon's run
  is `running`.
- When the daemon run terminates, the pipeline's run terminates with the
  **daemon's actual status** — `success`, `failure`, or `cancelled`.
- **Killing or cancelling the pipeline** (`POST /api/runs/{id}/kill`, or
  the engine cancelling the parent run) propagates to the live daemon
  stage: the daemon run transitions to `cancelled`, which becomes the
  pipeline's terminal status.

Whether the terminal stage is a daemon is decided from the **merged**
dispatch spec — a stage `trigger` override can flip daemon-ness either
way, and the pipeline behaves accordingly.

**Re-rendering a live daemon.** Re-firing any non-terminal stage of a
*live* pipeline (e.g. an operator runs `buildin/template` directly to
pick up rotated secrets) replays the descendant stages with fresh
`${input.…}` and then restarts the terminal daemon so it adopts the
freshly-rendered files. No pipeline restart or app restart is needed.

### Status semantics

| Situation | Pipeline run status |
| --- | --- |
| All stages succeed, terminal stage is **not** a daemon | `success` (return value = terminal stage's return) |
| All stages succeed, terminal stage **is** a daemon | `running` while the daemon runs, then the daemon's terminal status |
| Any stage reaches `failure` / `timeout` / `cancelled` | `failure`, with `fail_reason: stage N (<task-id>): <error>`; later stages never fire |
| Pipeline killed / cancelled while the daemon runs | `cancelled` |
| Overall `timeout:` exceeded | `failure` (the in-flight stage is cancelled) |

A pipeline's own return value is the **terminal stage's** return value,
persisted to the parent run row the same way a `kind: Task`'s is — so
chain consumers and `dicode.run_task` callers observe it. (The terminal
stage's `run_result.enabled: false` propagates, suppressing persistence
of secret-bearing returns.)

### How pipeline runs appear

Each pipeline fire produces **N+1** run rows:

- One **parent** run with `kind=pipeline` (the pipeline's own run).
- One **child** run per stage, each `kind=task`, linked by
  `parent_run_id`, with `trigger_source=pipeline-stage`.

The WebUI lists runs **per task** (`GET /api/tasks/{id}/runs` →
`Registry.ListRuns`, `WHERE task_id = ?` — there is no global list
filtered on `parent_run_id`). A pipeline fire shows up as the parent
run on the pipeline's own task; drill into it to fetch its stage
children (`GET /api/runs?parent=<id>` → `Registry.ListChildren`), which
are grouped under the parent with their individual statuses. A daemon
stage that fails shows up as a `failure` child under a `failure`
pipeline parent.

### Trigger types valid on a pipeline

A pipeline accepts `manual`, `cron`, `webhook` (with optional
`webhook_secret` / `auth`), and `chain`. It does **not** accept
`daemon` — daemon lifetime comes from the terminal stage, not the
pipeline trigger. As with `kind: Task`, at most one trigger type may be
set; omit `trigger` entirely for a pipeline fired only via
`dicode.run_task` or an outer pipeline.

### Pipeline vs. chain — when to use which

`kind: PipelineTask` does **not** replace `trigger.chain`. They model
orthogonal orchestration concerns and coexist:

| Concern | Pipeline (`kind: PipelineTask`) | Chain (`trigger.chain`) |
| --- | --- | --- |
| Coupling direction | The pipeline declares the sequence; stages don't know they're in one | The downstream declares its dependency on an upstream; the upstream is unaware |
| Style | Procedural — one file describes the whole flow | Event-driven / observer |
| Discoverability | Read one spec → see the entire flow | Scan the task graph for `chain.from: A`; fan-out is implicit |
| Coordination | One team owns the pipeline file | One team can react to another's task without editing it |
| Cardinality | One pipeline, N ordered stages | One source, M downstream subscribers (natural fan-out) |
| Failure semantics | A stage failure short-circuits the pipeline | `on_failure_chain` lets a separate task react to failure |

**Reach for a pipeline** when you own the whole sequence and want it
described in one place — especially the render → persist → start-daemon
shape.

**Reach for a chain** when:

1. **Decoupled observability/auditing** — task A runs; audit-task B
   chains from it without modifying A or every consumer of A.
2. **Failure remediation** — `on_failure_chain` runs B only when A
   fails; pipelines have no "run only if failed" grammar yet.
3. **Cross-team coordination** — team Y fires B when team X's task A
   completes, without coordinating a shared pipeline file.
4. **Many-to-one aggregation** — task C reacts when any of {A, B, D}
   completes (C has a `chain.from` on each).

**They compose, too:**

- **Chain → pipeline:** a `kind: PipelineTask` with
  `trigger.chain.from: <task>` fires when the upstream completes; the
  first stage receives the upstream's return via the same `${input.…}`
  grammar.
- **Pipeline → chain:** another task's `trigger.chain.from:
  <pipeline-id>` fires when the pipeline's **overall** run terminates
  (not on individual stage completion).
- **Stage-level `on_failure_chain`** still fires: a stage is a `kind:
  Task`, so its own configured failure chain fires when that stage
  fails, independent of the pipeline's short-circuit.

### Worked example: `buildin/relay-server`

A render → persist → start-daemon flow is modeled as a
`kind: PipelineTask` whose render/persist stages run first and whose
**terminal stage** is a standalone daemon-body `kind: Task`. The split
has two pieces:

1. A standalone **daemon-body** `kind: Task` (`trigger.daemon: true`)
   that reads its pre-rendered config off disk and is independently
   runnable.
2. A **`kind: PipelineTask`** whose render/persist stages run first and
   whose **terminal stage** is that daemon-body task. `${input.output}`
   threads each stage's return value into the next.

The pipeline (`task.yaml`) whose terminal stage is the daemon body:

```yaml
# tasks/buildin/relay-server/task.yaml  (kind: PipelineTask)
apiVersion: dicode/v1
kind: PipelineTask
name: Relay Server
description: Render relay.yaml from Doppler-fed env, then run the relay daemon.
subtype: sequential

stages:
  - task: buildin/template            # stage 1: render
    overrides:
      timeout: 30s
      params:
        - name: template_path
          default: "${TASK_DIR}/relay.yaml"
      fs:
        - path: "${TASK_DIR}/relay.yaml"
          permission: r
      env:
        - name: BASE_URL
          value: "https://relay.example.com"
        - name: STATUS_PASSWORD
          secret: RELAY_STATUS_PASSWORD   # local store, with a dev fallback
          default: "dicode-relay-dev"
        # ...optional, Doppler-fed OAuth client_id/secret entries...

  - task: buildin/write-local         # stage 2: persist stage 1's output
    overrides:
      timeout: 30s
      params:
        - name: content
          default: "${input.output}"
        - name: path
          default: "${DATADIR}/relay/relay.yaml"
        - name: mode
          default: "0600"
      fs:
        - path: "${DATADIR}/relay"
          permission: rw

  - task: buildin/relay-server-body   # terminal stage: the daemon
```

```yaml
# tasks/buildin/relay-server-body/task.yaml  (standalone daemon body)
apiVersion: dicode/v1
kind: Task
name: Relay Server (daemon body)
description: Runs the relay daemon, reading the pre-rendered relay.yaml off disk.
runtime: deno

trigger:
  daemon: true
  restart: always

permissions:
  net: ["*"]
  fs:
    - path: "${DATADIR}/relay"
      permission: rw
  env:
    - DICODE_DATADIR
    - DICODE_VERSION
```

Why it's shaped this way:

- The daemon body is a plain `kind: Task` that **reads** the
  pre-rendered config — it's independently runnable and carries no
  rendering concern. OAuth secrets and the status password are scoped to
  the render stage's `env`, never the daemon body's.
- The render and persist steps are `stages` in declaration order, each
  carrying its own per-edge overrides. `${input.output}` threads the
  rendered string from the template stage into the writer.
- Rotating Doppler secrets is straightforward: re-fire the render stage
  and the pipeline re-renders and restarts the terminal daemon.
- The render works without Doppler too: the OAuth `client_id` entries are
  `optional: true`, so when the provider is unavailable they degrade to
  empty and the template's `#dicode:if` guards drop the unconfigured
  provider blocks — leaving a valid `providers: {}` config. The status
  password comes from the local secrets store with a `default:` dev
  fallback, so a local operator can stand the relay up without Doppler.

For a full end-to-end docker variant, see the
[Cloudflare Tunnel worked example](../examples/cloudflare-tunnel.md).

---

## Docker runtime

Set `runtime: docker` to run a container instead of a JS script. No `task.js` is needed. Uses the Docker daemon via the Go SDK.

```yaml
name: Nginx Dev Server
description: Serves /tmp on port 8888. Kill from the run page when done.
runtime: docker

trigger:
  manual: true

docker:
  image: nginx:alpine
  pull_policy: missing       # always | missing (default) | never
  ports:
    - "8888:80"              # host:container
  volumes:
    - "/tmp:/usr/share/nginx/html:ro"
```

A more complete example:

```yaml
name: Data Pipeline
runtime: docker

trigger:
  cron: "0 3 * * *"

docker:
  image: python:3.12-slim
  command: ["python", "/scripts/pipeline.py"]
  pull_policy: missing
  volumes:
    - "/data/input:/input:ro"
    - "/data/output:/output"
  working_dir: /scripts
  env_vars:
    BATCH_SIZE: "500"
```

---

## Podman runtime

Set `runtime: podman` to run a rootless container via the `podman` CLI. Uses the same `docker:` config section as the Docker runtime — no changes to task fields required.

Podman must be installed on the host via the system package manager. dicode does not download it automatically, but the **Config → Runtimes** card will show its status and link to installation instructions.

```yaml
name: Nginx Dev Server
runtime: podman

trigger:
  manual: true

docker:
  image: nginx:alpine
  ports:
    - "8888:80"
  volumes:
    - "/tmp:/usr/share/nginx/html:ro"
```

**Differences from Docker:**

| | Docker | Podman |
| --- | --- | --- |
| Daemon required | Yes (`dockerd`) | No — daemonless, rootless by default |
| Go SDK | Yes | No — dicode uses the CLI |
| stdout/stderr | Multiplexed (Docker framing) | Plain line-by-line streams |
| Binary management | System / Docker Desktop | System package manager |

---

## Container fields (`docker:`)

Both the `docker` and `podman` runtimes share the same `docker:` config section.
Either `image` or `build` must be set — not neither.

### Pull a pre-built image

```yaml
docker:
  image: nginx:alpine
  pull_policy: missing   # always | missing (default) | never
```

### Build from a local Dockerfile

```yaml
docker:
  build:
    dockerfile: Dockerfile   # relative to task folder; default "Dockerfile"
    context: .               # relative to task folder; default task folder
  ports:
    - "8888:80"
```

The built image is tagged `dicode-<taskID>:<hash>` where `<hash>` is derived
from the Dockerfile content. If the Dockerfile hasn't changed, the existing image
is reused and the build step is skipped entirely. Build output is streamed to the
run log in real time.

Use **Edit code** on the task page to edit the Dockerfile directly in the web UI.

### Container fields reference

| Field | Type | Description |
| --- | --- | --- |
| `docker.image` | string | Container image (e.g. `nginx:alpine`). Required if `build` is not set. |
| `docker.build` | object | Build from local Dockerfile instead of pulling. |
| `docker.build.dockerfile` | string | Path to Dockerfile, relative to task folder. Default: `Dockerfile` |
| `docker.build.context` | string | Build context path, relative to task folder. Default: task folder |
| `docker.command` | list | Overrides image CMD |
| `docker.entrypoint` | list | Overrides image ENTRYPOINT |
| `docker.ports` | list | Port bindings — `"hostPort:containerPort"` |
| `docker.volumes` | list | Volume mounts — `"host:container[:ro]"`. Template vars `${DATADIR}`, `${TASK_DIR}`, `${HOME}` are expanded in the host path; `${UNKNOWN}` is left literal. Daemon env vars are NOT substituted. Bind-mounts of `/`, the docker/podman socket, and other sensitive host paths are **rejected by default** (see `container_security.allowed_volume_roots`). |
| `docker.working_dir` | string | Container working directory |
| `docker.env_vars` | map | Literal environment variables injected into container |
| `docker.pull_policy` | string | `missing` (default), `always`, `never`. Ignored when using `build`. |
| `docker.network_mode` | string | Container network — `bridge` (default for docker), `host`, `none`, or a user-defined network name. `host` (and `container:`/`ns:`) is **rejected by default** — allow via `container_security.allow_host_network`. |
| `docker.extra_hosts` | list | Extra `/etc/hosts` entries — `"<name>:<ip>"`. Use `host.docker.internal:host-gateway` to reach host services from a bridge-networked container. |
| `docker.cap_drop` | list | Linux capabilities to drop, e.g. `[ALL]`. |
| `docker.cap_add` | list | Linux capabilities to re-add after `cap_drop`. Escape-enabling caps (`SYS_ADMIN`, `SYS_PTRACE`, `SYS_MODULE`, `ALL`, …) are **rejected by default** — allow specific caps via `container_security.allowed_cap_add`. |
| `docker.security_opt` | list | Container security options, e.g. `["no-new-privileges:true"]`. Values that disable a sandbox layer (`seccomp=unconfined`, `apparmor=unconfined`, `label=disable`, `systempaths=unconfined`, `unmask=…`) are **rejected by default** — allow via `container_security.allow_insecure_security_opt`. |
| `docker.read_only` | bool | Mount the container rootfs read-only. Pair with explicit tmpfs/volumes for any paths that need writes. |
| `docker.user` | string | Run the container as `<uid>[:<gid>]` or `<name>[:<group>]`. Overrides the image's `USER` directive. |

> **Security floor.** The "rejected by default" values above are enforced as a hard, fail-closed floor in both the docker and podman runtimes (`pkg/runtime/containersec`) — the run is aborted before the container is created. Operators opt specific exceptions in via the top-level `container_security` block. See [Container Security Floor](security.md#container-security-floor).

> **Network isolation.** Docker and Podman tasks follow the same zero-default-permissions rule as Deno/Python: when `permissions.net` is empty and no `docker.ports` are published, the container starts with `network_mode: none` — no outbound connectivity. To allow network, add `permissions.net: ["*"]` (unrestricted) or specific hosts (allowed but not per-host enforced today; a warning is logged). Tasks that publish `docker.ports` always need a network interface and are not defaulted to `none`. An explicit `docker.network_mode` always takes precedence.

#### Hardened defaults

For daemon tasks that don't need root or host networking, a defense-in-depth baseline:

```yaml
docker:
  image: cloudflare/cloudflared:latest
  network_mode: bridge
  extra_hosts: ["host.docker.internal:host-gateway"]
  cap_drop: [ALL]
  security_opt: ["no-new-privileges:true"]
  read_only: true
  user: "65532:65532"   # nonroot
```

This keeps services bound to `127.0.0.1` on the host unreachable from the container, drops every capability, blocks setuid escalation, and runs unprivileged.

**Live logs** — container stdout/stderr is streamed line-by-line to the run log as it runs.

**Kill** — Container tasks may run indefinitely. Use the **Kill** button on the run detail page (or `POST /api/runs/{runID}/kill`) to stop the container gracefully (SIGTERM + 10 s timeout).

**No default timeout** — no runtime (Deno, Python, Docker, Podman) enforces a built-in default timeout. Set `timeout:` explicitly in `task.yaml` to bound run duration.

---

## `task.ts` / `task.js` (Deno runtime)

TypeScript or JavaScript. Runs via a managed Deno subprocess.

Globals available: `params`, `kv`, `input`, `state`, `output`, `mcp`, `dicode`. Use `console.log`/`warn`/`error`/`debug` for logging, native `fetch` for HTTP, and `Deno.env.get()` for declared env vars.

### Example

```javascript
// Read params and env
const channel = await params.get("slack_channel")
const token = Deno.env.get("SLACK_TOKEN")

// Fetch data
const res = await fetch("https://gmail.googleapis.com/gmail/v1/users/me/messages", {
  headers: { Authorization: `Bearer ${Deno.env.get("GMAIL_TOKEN")}` }
})

const { messages = [] } = await res.json()
console.log(`Found ${messages.length} messages`)

// Post to Slack
await fetch("https://slack.com/api/chat.postMessage", {
  method: "POST",
  headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
  body: JSON.stringify({
    channel,
    text: `You have ${messages.length} unread emails`
  })
})

// Return value available to chained tasks
return { count: messages.length }
```

### Constraints

- Filesystem access requires explicit `fs:` declarations in task.yaml
- Return value must be JSON-serializable (for chain triggers — capped at 1MB)
- Async/await and top-level await are supported

---

## `task.py` (Python runtime)

Python script executed via the managed [uv](https://github.com/astral-sh/uv) runner.
Install the Python runtime from **Config → Runtimes** before use.

```yaml
runtime: python
```

Params are available as `DICODE_PARAM_<NAME>` environment variables (name uppercased).
Inline dependencies via PEP 723 `# /// script` blocks are supported.

See [Python Runtime](../python-runtime.md) for full documentation.

---

## `task.test.js`

Unit test file. Uses a mock-aware test harness injected by the runtime.

See [Testing & Validation](./testing.md) for full documentation.

### Test example

```javascript
test("sends digest to slack on new emails", async () => {
  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [{ id: "1", snippet: "Hello from Alice" }] }
  })
  http.mock("POST", "https://slack.com/api/chat.postMessage", {
    status: 200,
    body: { ok: true }
  })
  env.set("GMAIL_TOKEN", "test-gmail-token")
  env.set("SLACK_TOKEN", "test-slack-token")
  params.set("slack_channel", "#test")

  const result = await runTask()

  assert.equal(result.count, 1)
  assert.httpCalled("POST", "https://slack.com/api/chat.postMessage")
})

test("handles empty inbox gracefully", async () => {
  http.mock("GET", "https://gmail.googleapis.com/*", {
    status: 200,
    body: { messages: [] }
  })
  env.set("GMAIL_TOKEN", "test-token")
  env.set("SLACK_TOKEN", "test-token")

  const result = await runTask()

  assert.equal(result.count, 0)
})
```

---

## Permissions

All access is **deny by default**. Tasks can only read env vars, touch the filesystem, or spawn subprocesses that are explicitly listed under `permissions:`.

```yaml
permissions:
  env:
    - SLACK_TOKEN               # bare: allowlist $SLACK_TOKEN from host env (same name)
    - name: API_KEY             # from: read $GH_TOKEN from host OS env, inject as API_KEY
      from: GH_TOKEN
    - name: DB_PASS             # secret: resolve "db_password" from secrets store
      secret: db_password
  net:
    - "api.github.com"          # restrict outbound to these hosts (omit = deny all)
  fs:
    - path: ~/data
      permission: r             # read-only
    - path: ~/reports
      permission: rw            # read + write + delete
  run:
    - curl                      # allow spawning curl
    # - "*"                     # allow all executables
  sys:
    - hostname                  # Deno system-info APIs (omit = deny all; Deno only)
```

### `permissions.env` — environment variables

Five forms, with clear source distinction:

| Form | Key | Source | Effect |
| --- | --- | --- | --- |
| Bare name | — | Host OS env | Script reads `$VAR` at runtime via `Deno.env.get()`; forwarded from the host if set |
| Bare pattern (`PREFIX_*`) | — | Host OS env | Forward **every** host var matching the trailing-`*` prefix, and make each matched name readable |
| `from:` | host OS var name | Host OS env | Read `$GH_TOKEN` from OS, inject subprocess env as `API_KEY` |
| `secret:` | secrets store key | Secrets store | Resolve encrypted secret, inject as the given name; **fails if not found** |
| `value:` | — | Literal | Inject a fixed string (used by taskset override layers) |

Two modifiers apply on top:

| Modifier | Applies to | Effect |
| --- | --- | --- |
| `optional: true` | `secret:` | Missing secret injects empty string instead of failing the run |
| `default: <literal>` | `secret:` | Missing secret injects this literal instead of failing (takes precedence over `optional:`); use for a documented dev fallback |
| `if_missing: { task: ... }` | `secret:` | Runs a prereq task (typically an OAuth flow) to populate the secret before dispatch |

**`from:` vs `secret:` — the key distinction:**
- `from:` reads **only** from the host OS environment (`os.Getenv`). Use it to rename a host env var or make the mapping explicit.
- `secret:` reads **only** from the dicode secrets store (set via `dicode secrets set`). Run fails immediately if the key is not in the store.
- A bare name does **neither** — it only allowlists the var so the script can read it from the host env via `Deno.env.get()`. No injection, no secrets lookup.
- A bare **pattern** (`PREFIX_*`) is a bare name with a trailing-`*` prefix glob: at launch it expands against the host environment, forwards every matching host var's value into the subprocess, and makes each matched name readable (Deno `--allow-env`, the Python env-read filter). It pulls in a related family of vars without enumerating each — while still **not** granting blanket read the way `env_read_exposed` does. Distinct from a lone `"*"` (rejected — use `env_read_exposed`).

> **Security:** a pattern never forwards the daemon's own credentials. `DICODE_MASTER_KEY`, `DICODE_API_KEY`, `DICODE_MCP_API_KEY` and the per-run IPC vars (`DICODE_SOCKET`/`DICODE_TOKEN`) are always excluded, even when a pattern like `DICODE_*` prefix-matches them.

#### Example 1 — bare passthrough (name stays the same)

```yaml
# task.yaml
permissions:
  env:
    - GITHUB_TOKEN
```

```typescript
// task.ts
export default async function main() {
  const token = Deno.env.get("GITHUB_TOKEN")  // reads $GITHUB_TOKEN from host env at runtime
}
```

#### Example 1b — pattern passthrough (forward a family of host vars)

The host OS has `GITHUB_TOKEN`, `GITHUB_SHA`, `GITHUB_REPOSITORY`. Forward all of
them without listing each:

```yaml
# task.yaml
permissions:
  env:
    - "GITHUB_*"   # every host var starting with GITHUB_ is forwarded and readable
```

```typescript
// task.ts
export default async function main() {
  const token = Deno.env.get("GITHUB_TOKEN")  // forwarded from the host
  const sha = Deno.env.get("GITHUB_SHA")
}
```

#### Example 2 — rename from host OS env

The host OS has `GH_TOKEN`. The script needs it as `GITHUB_TOKEN`.

```yaml
# task.yaml
permissions:
  env:
    - name: GITHUB_TOKEN   # name the script sees
      from: GH_TOKEN       # name in the host OS environment
```

```typescript
// task.ts
export default async function main() {
  const token = Deno.env.get("GITHUB_TOKEN")  // injected from $GH_TOKEN
}
```

#### Example 3 — inject from secrets store

Store first: `dicode secrets set slack_bot_token xoxb-…`

```yaml
# task.yaml
permissions:
  env:
    - name: SLACK_TOKEN        # name the script sees
      secret: slack_bot_token  # key in the dicode secrets store
```

```typescript
// task.ts
export default async function main() {
  const token = Deno.env.get("SLACK_TOKEN")  // resolved from secrets store
}
```

#### Example 4 — all forms together

```yaml
# task.yaml
permissions:
  env:
    - PORT                          # bare: script reads $PORT from host env directly
    - name: GITHUB_TOKEN            # from: rename $GH_TOKEN → GITHUB_TOKEN
      from: GH_TOKEN
    - name: SLACK_TOKEN             # secret: from encrypted secrets store
      secret: slack_bot_token
    - name: LOG_LEVEL               # value: literal (set by taskset override)
      value: "info"
  net:
    - "api.github.com"
    - "hooks.slack.com"
```

```typescript
// task.ts
export default async function main() {
  const port    = Deno.env.get("PORT")          // from host env (bare)
  const ghToken = Deno.env.get("GITHUB_TOKEN")  // injected, renamed from $GH_TOKEN
  const slToken = Deno.env.get("SLACK_TOKEN")   // injected from secrets store
  const level   = Deno.env.get("LOG_LEVEL")     // literal "info"
}
```

#### `env_read_exposed` — grant unrestricted env read (Deno node-compat / npm escape hatch; Python env-filter bypass)

`permissions.env_read_exposed: true` grants unrestricted env-var reads. For Deno it passes bare `--allow-env` to the sandbox. For Python it disables the `os.environ` filter (#418): by default Python tasks can initially only read the names listed in `permissions.env` plus a runtime-essential set (`PATH`, `HOME`, cache/proxy/TLS vars, `DICODE_SOCKET`, `DICODE_TOKEN`, …); keys the task writes with `os.environ["K"] = v` become readable immediately without this flag. Setting `env_read_exposed: true` lifts all restrictions.

The flag exists primarily for node-compat / npm tasks: `import "npm:…"` pulls in transitive dependencies that read `process.env` keys (`NODE_ENV` and others) at module-init time, before your `main()` runs. The set is unpredictable per dependency, so pinning individual names is fragile — `import "npm:dicode-relay/start"`, for example, still throws `NotCapable` even with `NODE_ENV` explicitly declared.

`env_read_exposed` widens *read permission* only. It is independent of the `env:` list — set the flag to allow reads, and keep the named/`secret:`/`from:` entries that your task actually needs **forwarded** (the flag grants permission to read a var but does not inject any values):

```yaml
permissions:
  env_read_exposed: true   # allow reading any env var (node-compat import needs this)
  env:
    - DICODE_DATADIR        # still forwarded so the script's value is populated
    - DICODE_VERSION
```

**Blast radius — why this is safe.** A task subprocess does not inherit the daemon environment. `runtime.SubprocessEnv` assembles the subprocess env as an allowlist: process basics + cache/proxy/TLS vars (`PATH`, `HOME`, `XDG_CACHE_HOME`, `DENO_DIR`, `HTTP(S)_PROXY`, `SSL_CERT_*`, …), the per-run IPC coordinates (`DICODE_SOCKET`/`DICODE_TOKEN`, which the task already holds), the host values of the task's own named entries, and its resolved secrets/values. The daemon master key and admin/MCP API keys are explicitly denylisted and never forwarded. So `env_read_exposed` lets the script read only that minimal, already-task-scoped env — it exposes nothing the task didn't already have.

#### Example 5 — `if_missing`: run a prereq task when a secret is absent

Useful when a secret is populated by an interactive flow (OAuth, device-code, etc.). If the secret is already in the store, the entry resolves normally and `if_missing` is a no-op. If it's missing, the trigger engine fires the declared prereq task in chain mode *before* the main task dispatches; if the prereq succeeds the secret is re-resolved and the main task runs.

```yaml
# ai-agent-openrouter — needs OPENROUTER_ACCESS_TOKEN, sourced via an OAuth flow
permissions:
  env:
    - name: OPENROUTER_ACCESS_TOKEN
      secret: OPENROUTER_ACCESS_TOKEN
      if_missing:
        task: auth/openrouter-oauth
        # params: { ... }       # optional, forwarded to the prereq task
```

If the prereq itself fails — for example an OAuth task throwing *"Open this URL to authorize: …"* — that error becomes the main task's failure, letting the UI (or the chat-bubble `renderError` helper) surface a clickable setup link. The same task can be both the first-time setup flow and the silent refresh path: well-designed OAuth tasks check expiry first, refresh silently when possible, and only throw the authorize URL when user action is actually required.

Only `secret:`-backed entries honor `if_missing:`. On a `from:`, `value:`, or bare entry the directive is silently ignored — there's no secret to check for presence in the first place.

### `permissions.fs` — filesystem access

| Permission | Read | Write | Delete | mkdir |
| --- | --- | --- | --- | --- |
| `r` | ✅ | ❌ | ❌ | ❌ |
| `w` | ❌ | ✅ | ✅ | ✅ |
| `rw` | ✅ | ✅ | ✅ | ✅ |

`~` is expanded to the user's home directory at runtime; relative paths resolve against the task directory.

Deno enforces both reads and writes via `--allow-read`/`--allow-write`. Python enforces **writes only** (via a PEP 578 audit hook): read allowlists are impractical in-interpreter — the interpreter and installed packages read files constantly — so reads stay unrestricted and `r` entries are ignored there.

Writes are denied by default: a Python task that writes a file, creates a directory or symlink, or uses the `tempfile` module must declare the target path with `w`/`rw`. Libraries that write under the temp directory therefore need an explicit grant, e.g. `- path: /tmp` with `permission: rw`.

### `permissions.run` — subprocess execution

Lists executables the script may spawn (`Deno.Command`, Python `subprocess`/`os.system`/`os.exec*`). Use `["*"]` to allow all. Omitting this field blocks all subprocess execution.

### `permissions.net` — outbound network

Controls which hostnames the task may connect to (Deno `--allow-net`; Python audit hook on `socket.connect`/`socket.getaddrinfo`).

| Value | Effect |
| --- | --- |
| Omit field (default) | Deny all outbound network |
| `["*"]` | Unrestricted outbound |
| `["api.github.com", "hooks.slack.com"]` | Restrict to listed hosts only |
| `[]` (empty list) | Deny all outbound network |

> **Migration note:** the default used to be unrestricted. Any existing task that makes outbound network calls but omits `permissions.net` must now declare it explicitly — `net: ["*"]` for arbitrary hosts, or a minimal host allowlist.

The Python enforcement is a guardrail, not a security boundary: hostnames are vetted at DNS resolution, so IP-literal connections pass the allowlist (deny-all still blocks them), and pooled or exotic async connections may not re-emit audit events.

### `permissions.sys` — system-info APIs (Deno only; Python ignores this field)

Controls access to Deno's [`Deno.systemMemoryInfo()`](https://deno.land/api?s=Deno.systemMemoryInfo), `Deno.hostname()`, etc.

| Value | Effect |
| --- | --- |
| Omit field (default) | Deny all sys APIs |
| `["hostname", "osRelease"]` | Allow listed APIs only |
| `["*"]` | Allow all sys APIs |

### `permissions.dicode` — dicode runtime API access

All `dicode.*` and `mcp.*` globals are **denied by default**. Each capability must be explicitly enabled under `permissions.dicode`.

| Field | What it enables |
| --- | --- |
| `tasks: ["task-id"]` | `dicode.run_task("task-id", …)` — only listed task IDs are callable; `["*"]` allows all |
| `mcp: ["daemon-id"]` | `mcp.list_tools()` and `mcp.call()` for the listed MCP daemon task IDs; `["*"]` allows all |
| `list_tasks: true` | `dicode.list_tasks()` |
| `get_runs: true` | `dicode.get_runs()` |
| `secrets_write: true` | `dicode.secrets_set(key, value)` and `dicode.secrets_delete(key)` — **write-only**, tasks can never read secrets back |
| `secrets_has: true` | `dicode.secrets.has(key)` — boolean presence check only; never reveals the secret value |
| `crypto: ["ctx"]` | `dicode.crypto.encrypt(ctx, data)` / `dicode.crypto.decrypt(ctx, blob)` — XChaCha20-Poly1305 encrypt/decrypt under a context-scoped, Argon2id-derived sub-key; `["*"]` allows all contexts. **Daemon-private contexts (currently `dicode/run-inputs/v1`) are always denied even when `["*"]` is granted.** |

```yaml
# An agent task that can call other tasks:
permissions:
  dicode:
    tasks:
      - send-report      # only this task ID is callable
      - notify-slack
    list_tasks: true     # can enumerate registered tasks

# Allow all task IDs and all MCP daemons:
permissions:
  dicode:
    tasks: ["*"]
    mcp: ["*"]

# A provisioning task that writes secrets programmatically:
permissions:
  dicode:
    secrets_write: true  # may set/delete secrets, never read them
```

> `dicode.run_task()` has a two-level check: the task must have the `tasks.trigger` capability (granted when `tasks:` is non-empty) **and** the specific task ID must be in the allowlist. The allowlist check is skipped only when `["*"]` is used.
>
> `secrets_write` intentionally has no read counterpart. Tasks access secrets at startup via `permissions.env` (injected before the script runs). A script cannot call `dicode.secrets_get()` — this is by design.

## Rich output types

Tasks can return typed output that renders nicely in the WebUI. Use the `output` global:

```javascript
// Default: JSON viewer
return { count: 5 }

// Rendered HTML (sandboxed iframe in WebUI)
return output.html(`<h1>Daily Report</h1><table>...</table>`)

// Plain text (monospace block)
return output.text("Done: processed 42 items\n3 errors")

// Image
return output.image("image/png", base64PngData)

// File download
return output.file("report.csv", csvContent, "text/csv")

// HTML with structured data for chain triggers
// chained tasks receive { count: 5 }, not the HTML
return output.html(htmlContent, { data: { count: 5 } })
```

See [Deno Runtime — `output`](../deno-runtime.md#output) for the full API.

## Suppressing return-value persistence

By default every successful task's return value is JSON-marshalled and written to the `runs.return_value` column. The WebUI, run-history API, WebSocket stream, and replay tooling all read from that column. For tasks that return secret-bearing material — a rendered config with embedded tokens, a freshly minted credential, a Doppler-resolved bundle — that persisted copy is a confidentiality leak.

Opt out at the task level:

```yaml
name: render-config
runtime: deno
trigger: { manual: true }
permissions:
  dicode: { secrets_has: true }
run_result:
  enabled: false      # do not persist return value to runs.return_value
```

What changes:

- The `runs.return_value` column is left empty for this task's runs.
- WebUI, REST `/api/runs`, and WebSocket payloads show no return value.
- Replay can re-fire the task (the input is still persisted unless `run_inputs.enabled: false` is also set) but won't have the previous return value to display.

What does **not** change:

- Synchronous callers of `dicode.run_task("render-config")` still receive the return value in-memory — the engine holds it briefly outside the DB until the waiting caller picks it up.
- Chain triggers (`trigger.chain.from: render-config`) still see the value as `input.output` in the downstream task — chain delivery never touches the persisted column.
- Structured rich-output payloads (`output.html(...)`, `output.image(...)`, `output.file(...)`) continue to persist normally, since they live in the `output_content` columns and aren't part of the return-value confidentiality concern. If those carry secrets too, route them through a different task or scrub before returning.
- `stdout`/`stderr` log lines persist normally. Combine with [`silent: true`](#) on the task spec when the script may print plaintext credentials.

**Security note:** this flag suppresses the persisted *return value* only. A task that wants to handle plaintext credentials end-to-end should typically combine `run_result.enabled: false`, `run_inputs.enabled: false`, `silent: true`, and the tightest possible `permissions.{net,fs,env}` allowlists to remove every exfiltration channel.

## Task ID

The task ID is derived from the folder name. It must be:

- Lowercase letters, digits, and hyphens only
- Unique across all configured sources
- Stable — changing the folder name changes the ID (breaks chain references and run history links)

Examples: `morning-email-check`, `github-release-notifier`, `backup-database`

---

## Template variables

Selected fields in `task.yaml` support `${VAR}` substitution, resolved at task-load time. Syntax is the standard shell form — no escaping, no pipes, no conditionals — just drop-in replacement.

### Supported fields

Template expansion runs over a tight allowlist, not every string field:

- `permissions.fs[].path`
- `trigger.webhook_secret`
- `permissions.env[].from`, `.secret`, `.value` (the indirection keys)
- `docker.volumes` (host-side mount paths; no env fallback)

Everything else — `name`, `description`, `params.*.default`, `system_prompt` defaults — is taken literally. This is deliberate: expansion in descriptive strings usually hides bugs rather than enabling them.

### Built-in variables

Always available inside a task:

| Variable | Value |
| --- | --- |
| `${TASK_DIR}` | Absolute path to this task's own directory (the one holding `task.yaml`) |
| `${HOME}` | User home directory (best-effort — may be unset in restricted environments) |

Injected by the source loader on every task it loads:

| Variable | Value |
| --- | --- |
| `${TASK_SET_DIR}` | Absolute path to the directory containing the root `taskset.yaml` of the source that loaded this task. Unset when the task is loaded outside of a source context (raw local folder source, unit test). |

See [../task-template-vars.md](../task-template-vars.md) for the per-field expansion policy (which task.yaml fields actually get `${VAR}` substituted, and the env-fallback rules that protect against daemon-secret exfiltration).

### Resolution order

1. Built-in variables (above), highest priority
2. Process environment (`os.Getenv`) — only for fields where env fallback is safe; see the linked policy doc
3. **Unresolved** — the literal `${VAR}` is left in place so bugs surface loudly instead of silently collapsing to an empty string

### Examples

Give a task read access to a shared skills directory one level above the taskset:

```yaml
permissions:
  fs:
    - path: "${TASK_SET_DIR}/../skills"
      permission: r
```

Reference a webhook secret from the process environment (closing the historical docs/reality gap where this was documented but never actually expanded):

```yaml
trigger:
  webhook: /hooks/github
  webhook_secret: "${GITHUB_WEBHOOK_SECRET}"
permissions:
  env:
    - GITHUB_WEBHOOK_SECRET
```

Compose a secrets-store key from a per-environment prefix:

```yaml
permissions:
  env:
    - name: DB_PASSWORD
      secret: "${DEPLOY_ENV}_db_password"
    - DEPLOY_ENV
```

---

## File layout rules

- `task.yaml` is always required. A folder without it is ignored.
- The script file (`task.ts`, `task.js`, or `task.py`) is required for code runtimes; omit it only for `runtime: docker` or `runtime: podman`.
- Container tasks using `docker.build` need a `Dockerfile` in the task folder (or at the path set in `docker.build.dockerfile`).
- `task.test.js` / `task.test.ts` is optional. `dicode task test` skips tasks without it.
- Any other files in the folder are ignored (useful for README, schema files, etc.).
- Subdirectories are ignored — task folders are flat.

---

## Configuration reference

For the full `dicode.yaml` configuration, see [Deployment](./deployment.md#configuration-reference).
