# Suspendable Tasks

A task can **pause mid-run to ask a human for input**, then continue once the
form is filled. Call `dicode.suspend({ state, form })`: the run ends as
`suspended`, dicode collects the input via the Web UI or the `dicode resume`
CLI, and the task runs again — this time with the answers in hand.

Use it for approval gates, wizards, "which of these do you mean?" disambiguation,
or any step that can't proceed without a person.

---

## The model — re-run, not freeze

Suspend is **not** VM suspension. Nothing is frozen in memory.

1. The task calls `dicode.suspend(...)`.
2. dicode records the `state` blob and the `form`, then the **process exits
   cleanly** (exit 0). The run row is marked `suspended`.
3. When someone submits the form, dicode starts a **brand-new process** for the
   same task and re-runs `task.ts` / `task.py` **from the top**.
4. On that re-run the task reads `resume_state` (the blob it passed to
   `suspend`) and `resume_input` (the submitted form values) to pick up where it
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
  state: unknown       // JSON-serializable; echoed back as resume_state on resume
  form: FormSchema     // the input to collect before resume
  deadline?: number    // optional Unix-ms instant; resumable until then (default: 24h)
}
```

Python signature (keyword args):

```python
dicode.suspend(state=<json>, form=<dict>, deadline=<unix_ms>)
```

### Reading the resume context

| | First run | Resume run |
|---|---|---|
| Deno global `resume_state` | `undefined` | the `state` you passed to `suspend()` |
| Deno global `resume_input` | `undefined` | the submitted form values, keyed by field `name` |
| Python `ctx.resume_state` | `None` | the `state` you passed to `suspend()` |
| Python `ctx.resume_input` | `None` | the submitted form values, keyed by field `name` |

`resume_input` values from the CLI are always strings; the Web UI submits the
value for the field's `type` (a `number` field submits a number, `boolean` a
bool). Coerce defensively.

`dicode.suspend` needs **no permission declaration** — it is granted by default
on the `deno` and `python` runtimes. It is **not** available on `docker` /
`podman` (see [Limits](#limits)).

---

## Form reference

### `FormSchema`

| Field | Type | Required | Description |
|---|---|---|---|
| `title` | string | no | Heading shown above the form |
| `description` | string | no | Sub-text under the title |
| `fields` | `FormField[]` | **yes** | The inputs to collect |

### `FormField`

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | **yes** | Key the answer appears under in `resume_input` |
| `label` | string | **yes** | Shown next to the input |
| `type` | `"string"` \| `"text"` \| `"number"` \| `"boolean"` \| `"select"` | **yes** | Which widget to render |
| `required` | boolean | no | Reject resume if left empty (a `boolean` counts as answered once present — `false` is a valid answer) |
| `default` | string \| number \| boolean | no | Pre-filled value |
| `options` | `{ value, label }[]` | for `select` | Choices; **required** when `type` is `"select"` |
| `placeholder` | string | no | Placeholder text |

### The five field types

```jsonc
// string — single-line text
{ "name": "title", "label": "Title", "type": "string", "required": true, "placeholder": "Weekly report" }

// text — multi-line textarea
{ "name": "notes", "label": "Notes", "type": "text", "placeholder": "Anything to add?" }

// number
{ "name": "count", "label": "How many?", "type": "number", "default": 10 }

// boolean — checkbox
{ "name": "notify", "label": "Send a notification?", "type": "boolean", "default": true }

// select — needs options
{
  "name": "env", "label": "Environment", "type": "select", "required": true,
  "options": [
    { "value": "staging", "label": "Staging" },
    { "value": "prod",    "label": "Production" }
  ]
}
```

---

## Worked example — a two-step wizard

Collect a project name, then a target environment, then finish. The task is a
state machine keyed by a `step` field carried in `state`.

### Deno (`task.ts`)

```typescript
type WizardState = { step: "name" } | { step: "env"; name: string }

const state = resume_state as WizardState | undefined
const answers = (resume_input ?? {}) as Record<string, string>

// First run — ask for the project name and pause.
if (!state) {
  await dicode.suspend({
    state: { step: "name" },
    form: {
      title: "New project",
      description: "What should we call it?",
      fields: [
        { name: "project", label: "Project name", type: "string", required: true },
      ],
    },
  })
  // unreachable — suspend() never returns
}

// Second run — the name is in, ask for the environment and pause again.
if (state.step === "name") {
  const project = answers.project ?? ""
  await dicode.suspend({
    state: { step: "env", name: project },
    form: {
      title: `Deploy ${project}`,
      fields: [
        {
          name: "env",
          label: "Target environment",
          type: "select",
          required: true,
          options: [
            { value: "staging", label: "Staging" },
            { value: "prod", label: "Production" },
          ],
        },
      ],
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
        form={
            "title": "New project",
            "description": "What should we call it?",
            "fields": [
                {"name": "project", "label": "Project name", "type": "string", "required": True},
            ],
        },
    )
    # unreachable — suspend() raises and the process exits

# Second run — the name is in, ask for the environment and pause again.
if state["step"] == "name":
    project = answers.get("project", "")
    dicode.suspend(
        state={"step": "env", "name": project},
        form={
            "title": f"Deploy {project}",
            "fields": [
                {
                    "name": "env",
                    "label": "Target environment",
                    "type": "select",
                    "required": True,
                    "options": [
                        {"value": "staging", "label": "Staging"},
                        {"value": "prod", "label": "Production"},
                    ],
                },
            ],
        },
    )

# Third run — both answers in hand; finish.
result = {"project": state["name"], "env": answers.get("env", "staging")}
```

Each `suspend()` mints its own resume, so a task can suspend any number of times
— that's what chains the wizard steps together.

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

Open the suspended run's detail page. It renders the `form` the task declared;
fill it in and submit. That POSTs the collected values to
`/api/runs/{runID}/resume`, which validates required fields and spawns the
continuation run. The raw resume handle is resolved server-side from the stored
run — the browser session (or API key) is the authorization.

### CLI

List what's waiting:

```console
$ dicode resume
RUN ID                               TASK                     SUSPENDED AT         FIELDS
b1c2…                                examples/deploy-wizard   2026-07-08T09:12:00Z project

resume with: dicode resume <run-id> [field=value ...]
```

Submit the answers as `field=value` pairs:

```console
$ dicode resume b1c2… project=acme-api
resumed: continuation run 7f8a…
follow: dicode logs 7f8a…
```

The continuation runs asynchronously; follow it with `dicode logs <run-id>`.
CLI values are always strings.

---

## See also

- [Task Format](./task-format.md) — `task.yaml`, params, permissions
- [Triggers](./triggers.md) — how a run is fired in the first place
- [Task → Orchestrator API](./orchestrator-api.md) — the rest of the `dicode` surface
