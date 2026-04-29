# task.yaml template variables

`task.yaml` supports `${VAR}` substitution in a small, carefully-chosen set of
fields. Variables are resolved at spec-load time; unresolved references are
left as literal `${VAR}` so a typo produces an obvious failure instead of
silently collapsing to `""`.

## Built-in variables

| Variable        | Value                                                                 | Available when                                                      |
| --------------- | --------------------------------------------------------------------- | ------------------------------------------------------------------- |
| `TASK_DIR`      | Absolute path of this task's own directory (the one holding `task.yaml`). | Always.                                                             |
| `HOME`          | Home directory of the user running the dicode daemon.                | Always (best-effort — absent on systems where `UserHomeDir` fails). |
| `TASK_SET_DIR`  | Absolute path of the directory containing the root `taskset.yaml` of the source that loaded this task. | Only when the task is loaded via a TaskSet source. Absent for raw local/git folder sources and unit tests. |
| `DATADIR`       | Absolute path of the daemon's data directory (`config.DataDir`, e.g. `~/.dicode`). | Always (when loaded by the daemon). |

There is no `SKILLS_DIR`, `CONFIG_DIR`, or `SOURCE_ROOT` — use `TASK_DIR` and
`TASK_SET_DIR` to derive the paths you need. For example, a shared `skills/`
directory one level above a taskset is `${TASK_SET_DIR}/../skills`.

## Fields that accept template expansion

Expansion is deliberately **not** applied to every string field — most
(`name`, `description`, `system_prompt` defaults, …) should be taken
literally. These are the fields that get expanded:

| Field                       | Env fallback? | Notes                                                                                                                   |
| --------------------------- | ------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `trigger.webhook_secret`    | Yes           | HMAC secret used server-side; task code never sees it. Env fallback lets you reference `${WEBHOOK_SECRET}` from the daemon env. |
| `permissions.fs[].path`     | Yes           | Consumed by the Deno permission set. `${HOME}/shared`, `${TASK_SET_DIR}/skills`, etc.                                   |
| `permissions.env[].from`    | Yes           | Host env-var name to rename/inject. Identifier, not a value — safe to env-fallback.                                     |
| `permissions.env[].secret`  | Yes           | Secrets-store key to look up. Identifier, not a value.                                                                  |
| `permissions.env[].value`   | **No**        | Literal value injected into `Deno.env.get(name)` at runtime. Env fallback would be a direct exfiltration primitive.     |
| `params[].default`          | **No**        | Surfaces loader-provided paths as parameter defaults task code reads via `params.get()`. Same reason as `env.value`.    |

"Env fallback = Yes" means: if the variable is not a known builtin, the
loader falls back to `os.Getenv(name)`. This is *off* for any field whose
value is readable from inside the task sandbox — otherwise a task.yaml from
an untrusted source could exfiltrate daemon secrets by naming them as
template variables.

## Unresolved references

If a variable is not found in the builtin set, not in loader extras, and
(where allowed) not in the process environment, the literal `${VAR}` is
preserved. That makes typos and missing-context bugs loud rather than
silently producing an empty or malformed path.

## Adding a new variable

1. Add a `Var*` constant in [pkg/task/template.go](../pkg/task/template.go).
2. Wire it up wherever the loader has the value (see `pkg/taskset/source.go`
   and `pkg/source/{local,git}` for the injection point).
3. Add a row to the table above.
4. Add a test in [pkg/task/template_test.go](../pkg/task/template_test.go).

Keep the set small — every new builtin is a new thing users have to learn,
and the whole point of the template system is that it's narrow and
predictable.
