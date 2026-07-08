# Suspendable Tasks

A task can **pause mid-run to ask a human for input**, then continue once the
form is filled. Call `dicode.suspend({ state, schema })`: the run ends as
`suspended`, dicode collects the input via the Web UI or the `dicode resume`
CLI, validates it against the schema, and the task runs again — this time with
the answers in hand.

Use it for approval gates, wizards, "which of these do you mean?" disambiguation,
or any step that can't proceed without a person.

The form is described with a **[JSON Schema](https://json-schema.org)** (draft
2020-12). That is a standard, portable vocabulary: the same schema drives the
default Web UI form, the CLI prompts, and the **server-side validation** that
guarantees `resume_input` conforms before your task re-runs.

---

## The model — re-run, not freeze

Suspend is **not** VM suspension. Nothing is frozen in memory.

1. The task calls `dicode.suspend(...)`.
2. dicode records the `state` blob and the `schema`, then the **process exits
   cleanly** (exit 0). The run row is marked `suspended`.
3. When someone submits the form, dicode **validates the submission against the
   schema**, then starts a **brand-new process** for the same task and re-runs
   `task.ts` / `task.py` **from the top**.
4. On that re-run the task reads `resume_state` (the blob it passed to
   `suspend`) and `resume_input` (the submitted values) to pick up where it
   left off.

Because the whole file runs again from the top, your task owns its control flow:
**branch on `resume_state` early** to decide which step you're on. Any side
effect before the `suspend()` call runs again on every resume, so keep the
pre-suspend section idempotent (or gate it behind a `resume_state` check).

A first (non-resume) run has `resume_state` / `resume_input` **undefined**
(Deno) or **`None`** (Python) — that's how you detect the initial pass.

---

## The API

### `dicode.suspend(req)`

```typescript
dicode.suspend(req: SuspendRequest): Promise<never>
```

Pauses the run. It records `req` over IPC, then **never returns** — internally
it throws a control signal that the runtime catches to exit the process. Code
after `suspend()` does not run on the suspending pass; it runs on the resume
pass instead (from the top of the file).

```typescript
interface SuspendRequest {
  state?: unknown      // JSON-serializable; echoed back as resume_state on resume
  schema: JSONSchema   // JSON Schema (draft 2020-12) for the input to collect
  deadline?: number    // optional Unix-ms instant; resumable until then (default: 24h)
}
```

Python signature (keyword args):

```python
dicode.suspend(state=<json>, schema=<dict>, deadline=<unix_ms>)
```

### Reading the resume context

| | First run | Resume run |
|---|---|---|
| Deno global `resume_state` | `undefined` | the `state` you passed to `suspend()` |
| Deno global `resume_input` | `undefined` | the submitted values, keyed by property name |
| Python `ctx.resume_state` | `None` | the `state` you passed to `suspend()` |
| Python `ctx.resume_input` | `None` | the submitted values, keyed by property name |

`resume_input` is validated against the schema before your task re-runs, and its
values arrive with the **declared JSON types** — a `number`/`integer` property
is a number, a `boolean` is a bool. (Coerce defensively anyway.)

`dicode.suspend` needs **no permission declaration** — it is granted by default
on the `deno` and `python` runtimes. It is **not** available on `docker` /
`podman` (see the Deno / Python only note under [Rules and gotchas](#rules-and-gotchas)).

---

## The form — JSON Schema

`schema` is a JSON Schema object describing the **input object** the user fills
in. Server-side validation rejects a submission that doesn't conform (Web UI:
`400` with the failing property; CLI: a clear error) before the task resumes.

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

## Worked example — a two-step wizard

Collect a project name, then a target environment, then finish. The task is a
state machine keyed by a `step` field carried in `state`.

### Deno (`task.ts`)

```typescript
type WizardState = { step: "name" } | { step: "env"; name: string }

const state = resume_state as WizardState | undefined
const answers = (resume_input ?? {}) as Record<string, unknown>

// First run — ask for the project name and pause.
if (!state) {
  await dicode.suspend({
    state: { step: "name" },
    schema: {
      type: "object",
      title: "New project",
      description: "What should we call it?",
      properties: {
        project: { type: "string", title: "Project name" },
      },
      required: ["project"],
    },
  })
  // unreachable — suspend() never returns
}

// Second run — the name is in, ask for the environment and pause again.
if (state.step === "name") {
  const project = String(answers.project ?? "")
  await dicode.suspend({
    state: { step: "env", name: project },
    schema: {
      type: "object",
      title: `Deploy ${project}`,
      properties: {
        env: { type: "string", title: "Target environment", enum: ["staging", "prod"] },
      },
      required: ["env"],
    },
  })
}

// Third run — both answers in hand; finish.
return { project: state.name, env: answers.env ?? "staging" }
```

### Python (`task.py`)

```python
state = ctx.resume_state
answers = ctx.resume_input or {}

# First run — ask for the project name and pause.
if state is None:
    dicode.suspend(
        state={"step": "name"},
        schema={
            "type": "object",
            "title": "New project",
            "description": "What should we call it?",
            "properties": {"project": {"type": "string", "title": "Project name"}},
            "required": ["project"],
        },
    )
    # unreachable — suspend() raises and the process exits

# Second run — the name is in, ask for the environment and pause again.
if state["step"] == "name":
    project = answers.get("project", "")
    dicode.suspend(
        state={"step": "env", "name": project},
        schema={
            "type": "object",
            "title": f"Deploy {project}",
            "properties": {
                "env": {"type": "string", "title": "Target environment", "enum": ["staging", "prod"]},
            },
            "required": ["env"],
        },
    )

# Third run — both answers in hand; finish.
result = {"project": state["name"], "env": answers.get("env", "staging")}
```

Each `suspend()` mints its own resume, so a task can suspend any number of times
— that's what chains the wizard steps together. (Each step still branches on
`resume_state` by hand; automatic step dispatch is a separate follow-up.)

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
  `ctx.params` is unchanged), but the `input` that fired the *original* run (the
  webhook body, the chain payload) is **not** replayed. If you'll need something
  from `input` after resuming, **stash it into `state`** before you suspend.

- **`deadline` is optional.** It's a Unix-ms instant; the run stays resumable
  until then. Omit it (or pass `0`) for the default **24-hour** window. Once the
  deadline lapses, the sweep cancels the run (status `cancelled`, fail reason
  `resume_timeout`) and any later resume attempt is rejected.

- **A suspended run resumes exactly once.** The resume handle is single-use and
  consumed atomically — a second resume of the same run fails with "already
  resumed".

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
continuation carries on from `resume_state`.

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
