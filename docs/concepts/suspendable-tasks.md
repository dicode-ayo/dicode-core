# Suspendable Tasks

A task can **pause mid-run to ask a human for input**, then continue once the
form is filled. Call `dicode.suspend({ schema })`: the run ends as `suspended`,
dicode collects the input via the Web UI or the `dicode resume` CLI, validates
it against the schema, and the task runs again — this time with the answers in
hand, dispatched to a **resume handler you export** so you never write an
`if (resume_state)` switch.

Use it for approval gates, wizards, "which of these do you mean?" disambiguation,
or any step that can't proceed without a person.

The form is described with a **[JSON Schema](https://json-schema.org)** (draft
2020-12). That is a standard, portable vocabulary: the same schema drives the
default Web UI form, the CLI prompts, and the **server-side validation** that
guarantees `ctx.input` conforms before your task re-runs.

---

## The model — re-run, not freeze

Suspend is **not** VM suspension. Nothing is frozen in memory.

1. The task calls `dicode.suspend(...)`.
2. dicode records the carried `state` blob and the `schema`, then the **process
   exits cleanly** (exit 0). The run row is marked `suspended`.
3. When someone submits the form, dicode **validates the submission against the
   schema**, then starts a **brand-new process** for the same task and re-runs
   `task.ts` / `task.py` **from the top**.
4. On that re-run the runner **dispatches the right handler** and hands it
   `ctx.state` (the blob you carried) and `ctx.input` (the submitted values).

Because the whole file re-runs from the top, any side effect at module top level
runs again on every resume — keep top-level code idempotent and put real work
inside a handler. You do **not** write a step switch: the runner picks the
handler for you (see [Auto-dispatch](#the-handlers--auto-dispatch)).

A first (non-resume) run sees `ctx.state` **undefined** (Deno) / **`None`**
(Python) in `main`; a resumed handler sees the real carried blob.

---

## The API

### `dicode.suspend(req)`

```typescript
dicode.suspend(req: SuspendRequest): Promise<never>
```

Pauses the run. It records `req` over IPC, then **never returns** — internally
it throws a control signal that the runtime catches to exit the process. Code
after `suspend()` does not run on the suspending pass; it runs on the resume
pass instead (in the handler the runner dispatches to).

```typescript
interface SuspendRequest {
  schema: JSONSchema   // JSON Schema (draft 2020-12) for the input to collect
  to?: string          // name of the steps[] handler to run on resume (wizard shape)
  state?: unknown      // JSON-serializable blob carried to the resume as ctx.state
  deadline?: number    // optional Unix-ms instant; resumable until then (default: 24h)
}
```

- **`to`** names the `steps` handler to dispatch on resume — the wizard shape.
  Omit it for the two-function (`main` + `resume`) shape.
- **`state`** is your own carried blob, handed back as `ctx.state` on the
  resume. The runner persists it wrapped with an internal step marker (so
  first-vs-resume stays unambiguous) — you never see or manage that marker.

Python signature (keyword args):

```python
dicode.suspend(schema=<dict>, to=<str>, state=<json>, deadline=<unix_ms>)
```

### The handlers — auto-dispatch

You export handlers; the runner picks which one runs based on the resume:

| You export | First run | Resume |
|---|---|---|
| `main` only | `main` | `main` again (branch on `ctx.state` by hand) |
| `main` + `resume` | `main` | `resume` |
| `main` + `steps` map | `main` | `steps[to]` — the handler named by `suspend({ to })` |

`main` is always the entry (first) step. Each handler receives the resume
context — **Deno**: destructure the argument (`async function resume({ state, input, dicode })`);
**Python**: read the module-global `ctx` (`ctx.state`, `ctx.input`) or accept it
as an argument (`async def resume(ctx):`).

| | First run | Resume run |
|---|---|---|
| `ctx.state` | `undefined` / `None` | the `state` you passed to `suspend()` (unwrapped) |
| `ctx.input` | the trigger input | the submitted values, keyed by property name |

`ctx.input` on a resume is validated against the schema before your task
re-runs, and its values arrive with the **declared JSON types** — a
`number`/`integer` property is a number, a `boolean` is a bool. (Coerce
defensively anyway.)

`dicode.suspend` needs **no permission declaration** — it is granted by default
on the `deno` and `python` runtimes. It is **not** available on `docker` /
`podman` (see the Deno / Python only note under [Rules and gotchas](#rules-and-gotchas)).

---

## The form — JSON Schema

`schema` is a JSON Schema object describing the **input object** the user fills
in. Server-side validation rejects a submission that doesn't conform (Web UI:
`400` with the failing property; CLI: a clear error) before the task resumes.

The daemon compiles the schema the moment you call `dicode.suspend`, so a
malformed schema surfaces as an error to the task immediately — while it can
still react — instead of silently suspending and then failing every resume
attempt. External or `file://` `$ref`s are refused; a suspend schema must be
self-contained.

Use an `object` schema whose `properties` are the fields to collect:

```jsonc
{
  "type": "object",
  "title": "Deploy",                 // heading shown above the form
  "description": "Pick a target.",   // sub-text under the title
  "properties": {
    "project": { "type": "string", "title": "Project name" },
    "notes":   { "type": "string", "title": "Notes", "format": "textarea" },
    "count":   { "type": "integer", "title": "How many?", "default": 10 },
    "notify":  { "type": "boolean", "title": "Send a notification?", "default": true },
    "env":     { "type": "string", "title": "Environment", "enum": ["staging", "prod"] }
  },
  "required": ["project", "env"]
}
```

### What the default renderer maps

The built-in Web UI form renders the common JSON-Schema subset with zero author
effort. Each property in `properties` becomes one control:

| Schema | Control |
|---|---|
| `type: "string"` | text input |
| `type: "string"`, `format: "textarea"` (or `"multiline"`) | textarea |
| `enum: [...]` (any type) | select |
| `type: "boolean"` | checkbox |
| `type: "number"` / `"integer"` | number input |

Honored keywords: `title` (label — falls back to the property name),
`description` (help text), `default` (pre-filled value), `enum` (choices),
and the top-level `required` array (marks a field required and drives the
missing-field validation). Standard constraint keywords (`minimum`,
`maxLength`, `pattern`, …) are enforced by the server-side validator even when
the default renderer has no special widget for them.

The renderer covers the 90% case; anything the schema expresses is still
validated server-side regardless of how it renders.

---

## Worked example — a two-step wizard (`steps`)

Collect a project name, then a target environment, then finish. `suspend({ to })`
names the next handler; the runner dispatches it and hands it `ctx.input` (the
submission) and `ctx.state` (the blob you carried). No step switch.

### Deno (`task.ts`)

```typescript
export default async function main({ dicode }) {
  // First run — ask for the project name, resume into the "env" step.
  await dicode.suspend({
    to: "env",
    schema: {
      type: "object",
      title: "New project",
      description: "What should we call it?",
      properties: { project: { type: "string", title: "Project name" } },
      required: ["project"],
    },
  })
  // unreachable — suspend() never returns
}

export const steps = {
  // Runs on the first resume: the name is in ctx.input; ask for the environment,
  // carrying the name forward in state.
  async env({ dicode, input }) {
    await dicode.suspend({
      to: "finish",
      state: { name: input.project },
      schema: {
        type: "object",
        title: `Deploy ${input.project}`,
        properties: {
          env: { type: "string", title: "Target environment", enum: ["staging", "prod"] },
        },
        required: ["env"],
      },
    })
  },
  // Runs on the second resume: both answers in hand; finish.
  async finish({ state, input }) {
    return { project: state.name, env: input.env ?? "staging" }
  },
}
```

### Python (`task.py`)

```python
async def main():
    # First run — ask for the project name, resume into the "env" step.
    dicode.suspend(
        to="env",
        schema={
            "type": "object",
            "title": "New project",
            "description": "What should we call it?",
            "properties": {"project": {"type": "string", "title": "Project name"}},
            "required": ["project"],
        },
    )
    # unreachable — suspend() raises and the process exits


async def env(ctx):
    # First resume: the name is in ctx.input; ask for the environment, carrying
    # the name forward in state.
    dicode.suspend(
        to="finish",
        state={"name": ctx.input["project"]},
        schema={
            "type": "object",
            "title": f"Deploy {ctx.input['project']}",
            "properties": {
                "env": {"type": "string", "title": "Target environment", "enum": ["staging", "prod"]},
            },
            "required": ["env"],
        },
    )


async def finish(ctx):
    # Second resume: both answers in hand; finish.
    return {"project": ctx.state["name"], "env": ctx.input.get("env", "staging")}


steps = {"env": env, "finish": finish}
```

Each `suspend()` mints its own resume, so a task can suspend any number of times
— chaining the wizard steps together, one named handler per pause.

## Simpler — ask once, then finish (`resume`)

When you only pause once, skip the `steps` map: export `main` and `resume`. The
runner runs `main` on the first pass and `resume` on the continuation.

```typescript
export default async function main({ dicode }) {
  await dicode.suspend({
    schema: {
      type: "object",
      title: "Approve deploy?",
      properties: { ok: { type: "boolean", title: "Ship it?" } },
      required: ["ok"],
    },
  })
}

export async function resume({ input }) {
  return { shipped: input.ok }
}
```

A task that exports **only `main`** still works: on resume the runner re-runs
`main`, and you branch on `ctx.state` by hand (undefined on the first run, your
carried blob on the resume). Pass a `state` when you go this route so the two
passes are distinguishable.

---

## Try the shipped example

A ready-to-run wizard ships under
[`tasks/examples/suspend-wizard/`](../../tasks/examples/suspend-wizard/). It is a
manually-triggered, three-step "new project" wizard built on the `steps` map and
the JSON-Schema resume form — the same shape as the worked example above, one hop
longer.

1. In the Web UI, open the **Suspend Wizard (example)** task and **Run** it. The
   run immediately goes `suspended` and its detail page renders a form.
2. **Step 1 — Project name.** `main` suspends `to: "chooseFramework"` asking for a
   `project_name` (string, required). Fill it in and submit.
3. **Step 2 — Framework.** `chooseFramework` reads the name from `ctx.input`,
   carries it in `state`, and suspends `to: "confirm"` asking for a `framework`
   (an `enum` of `deno` / `node` / `bun`, rendered as a select).
4. **Step 3 — Confirm.** `confirm` suspends `to: "summarize"` with a `confirmed`
   boolean (a checkbox), carrying name + framework forward.
5. **Finish.** `summarize` returns `{ project, framework, confirmed }` — the run
   ends `resumed` and the continuation's result is on its run detail page.

Each pause auto-renders from the schema, so there is no form code to write; the
runner dispatches the next handler by the `to` name with no step switch. The same
flow works from the CLI — `dicode resume` lists the suspended run and
`dicode resume <run-id> project_name=acme` (etc.) submits each step.

### A branching example

[`tasks/examples/deploy-wizard/`](../../tasks/examples/deploy-wizard/) goes
further: a deploy wizard where each step runs real **async work** (a candidate
scan, a check run, the deploy itself — stand-ins that log progress) and the
**next step is chosen at runtime**. Because `to` is a plain value, `if/else` on an
async result or on `ctx.input` picks the branch: a failed coverage gate suspends
to an override prompt, `prod` takes an extra confirmation, and three paths
converge on the same deploy step. It needs no network or secrets. Same primitives
as above — the only new idea is that the step graph is decided by code, not
declared.

---

## Rules and gotchas

- **`state` must be JSON-serializable.** It is persisted as JSON, so functions,
  class instances, `Map`/`Set`, etc. won't survive the round trip. Keep it to
  plain objects, arrays, strings, numbers, and booleans.

- **Never swallow the suspend signal.** `suspend()` throws (Deno) / raises
  (Python) a control signal *after* the payload is recorded; the runtime wrapper
  catches it to end the run cleanly. If your own `try/catch` (or bare
  `except:`) swallows it and the task keeps running or returns normally, the
  wrapper detects the contradiction — a run that both suspended and returned —
  and **fails the run loudly**. Don't wrap `suspend()` in a catch-all, or
  re-throw the signal if you must.

- **`params` survive a resume; the original trigger input does not.** The
  continuation is a fresh run that restores the suspended run's `params` (so
  `ctx.params` is unchanged). On a resume `ctx.input` is the **form submission**,
  not the payload that fired the *original* run (the webhook body, the chain
  payload) — that is **not** replayed. If you'll need something from the original
  input after resuming, **stash it into `state`** before you suspend.

- **`deadline` is optional.** It's a Unix-ms instant; the run stays resumable
  until then. Omit it (or pass `0`) for the default **24-hour** window. Once the
  deadline lapses, the sweep cancels the run (status `cancelled`, fail reason
  `resume_timeout`) and any later resume attempt is rejected.

- **A suspended run resumes exactly once.** The resume handle is single-use and
  consumed atomically — a second resume of the same run fails with "already
  resumed".

- **`suspend({ to })` must name a handler in the exported `steps` map.** If it
  names a step that isn't present (a typo, or a step you removed while a run was
  mid-wizard), the resume **fails loudly** with a clear error and a non-zero exit
  rather than silently falling back to `main`/`resume` against mid-wizard state.
  The single-`main` and `main` + `resume` shapes (which export no `steps` map)
  are unaffected.

- **Deno / Python only.** `docker` and `podman` tasks cannot suspend; a
  `suspend()` attempt there fails with a permission-denied error rather than
  being silently dropped.

---

## Lifecycle and statuses

```
running ──suspend()──► suspended ──resume──► resumed        (terminal)
                            │
                            └──deadline lapse──► cancelled   (fail_reason: resume_timeout)
```

| Status | Terminal? | Meaning |
|---|---|---|
| `suspended` | no | Paused awaiting input. `finished_at` stays NULL. |
| `resumed` | yes | The resume handle was consumed and a continuation run was spawned. Set exactly once. |
| `cancelled` (`resume_timeout`) | yes | The `deadline` lapsed before anyone resumed. |

The **continuation is a new run** with its own run id, taken through the normal
execution path — which is why it can suspend again. The original suspended run
does not itself "become" the continuation; it transitions to `resumed` and the
continuation carries on from the handler the runner dispatches, with `ctx.state`
restored.

---

## How to resume

### Web UI

Open the suspended run's detail page. It renders a form from the JSON Schema the
task declared; fill it in and submit. That POSTs the collected values to
`/api/runs/{runID}/resume`, which **validates them against the stored schema**
and spawns the continuation run. The raw resume handle is resolved server-side
from the stored run — the browser session (or API key) is the authorization.

### CLI

List what's waiting (the `FIELDS` column shows the schema's required
properties):

```console
$ dicode resume
RUN ID                               TASK                     SUSPENDED AT         FIELDS
b1c2…                                examples/deploy-wizard   2026-07-08T09:12:00Z project,env
```

Resume interactively — dicode reads the schema and prompts for each property,
honoring its type, `enum` choices, `required`, and `default`:

```console
$ dicode resume b1c2…
Project name (required): acme-api
Target environment
  choices: staging, prod
env (required): prod
resumed: continuation run 7f8a…
```

Or pass the answers as `field=value` pairs — each value is coerced to the
property's declared type and validated against the schema before submit:

```console
$ dicode resume b1c2… project=acme-api env=prod
resumed: continuation run 7f8a…
follow: dicode logs 7f8a…
```

The continuation runs asynchronously; follow it with `dicode logs <run-id>`.

---

## See also

- [Task Format](./task-format.md) — `task.yaml`, params, permissions
- [Triggers](./triggers.md) — how a run is fired in the first place
- [Task → Orchestrator API](./orchestrator-api.md) — the rest of the `dicode` surface
