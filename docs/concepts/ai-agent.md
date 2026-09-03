# AI Agent

The **ai-agent** built-in task gives dicode a full chat interface backed by any OpenAI-compatible model. The agent can call your other dicode tasks as tools, persist conversations across turns, and look up markdown "skills" for domain context.

Unlike the existing "AI generates code" feature (see [docs/concepts/ai-generation.md](ai-generation.md)), which uses an AI model to *write* tasks, ai-agent uses an AI model to *orchestrate* tasks you already have. The two features complement each other: one is about authoring, one is about operation.

---

## What you get

- **A chat page** at `/hooks/ai` (plus per-provider presets at `/hooks/ai/ollama`, `/hooks/ai/openai`, `/hooks/ai/groq`). Send a message, get a reply. Sessions persist across turns.
- **Tool use** — the model sees every registered task as a callable tool. Ask "check my weekly-report runs from last month" and the agent calls `dicode.get_runs("weekly-report")` via the corresponding task and answers based on the actual data, not a hallucination.
- **Skills** — drop markdown files into `dicode-buildin/skills/` and pass their names via the `skills` param. The agent is told each skill's name and description and reads the ones it needs through the `dicode_read_skill` tool. Use them to give the agent durable context it should know about every time (domain glossary, team conventions, current priorities).
- **Session persistence** — each conversation is keyed by `session_id` and stored in the task's KV store. Hybrid id model: pass your own, or omit it to have the task generate and return one.
- **Lazy compaction** — when the conversation exceeds `max_history_tokens`, older turns are replaced by a running summary generated via a second model call. Controlled by the `compaction_model` param (defaults to the main model).
- **Provider-agnostic** — works with OpenAI, Anthropic (via openai-compat), Ollama, LM Studio, Groq, OpenRouter, Together, DeepSeek, and any other endpoint that speaks the OpenAI chat completions API. Pick your provider via taskset overrides.

---

## Shape of a conversation

```
browser ──POST /hooks/ai {prompt, session_id?}──▶ ai-agent task
                                                        │
                                           load kv[chat:session_id]
                                                        │
                                       append user message to history
                                                        │
                             list_tasks → build OpenAI tool schema
                                                        │
                                  ◀── model.chat.completions.create
                                                        │
                            tool_calls? ──yes──▶ run_task(id, args)
                                                        │
                                      append tool response, loop
                                                        │
                             tool_calls? ──no──▶ return {session_id, reply}
                                                        │
                                    save kv[chat:session_id]
```

Request:

```json
{
  "prompt": "How many weekly-report runs failed this month?",
  "session_id": "optional — omit to auto-generate"
}
```

Response:

```json
{
  "session_id": "e4b9f3a2-...",
  "reply": "You had 3 failed weekly-report runs this month, all on ..."
}
```

When the buildin is called without a configured provider, the turn is terminal: it fails the run (HTTP 500) rather than settling as a successful one carrying the misconfiguration as prose in `reply`. The structured envelope is still delivered as the response body — the daemon captures it via `output.json` before the non-zero exit — so a webhook caller and the chat UI can both render a clear message instead of reading a green run:

```json
{
  "session_id": "e4b9f3a2-...",
  "reply": "not configured — missing model, base_url. This is the generic ai-agent buildin...",
  "error": "not_configured",
  "missing": ["model", "base_url"],
  "hint": "This is the generic ai-agent buildin. It has no provider configured..."
}
```

---

## Tools vs skills

These are two different concepts that dicode uses with specific meanings:

| Concept | What it is | Where it lives | How the agent sees it |
| ------- | ---------- | -------------- | --------------------- |
| **Tool** | A dicode task the agent can execute | `tasks/**/task.yaml` | As an OpenAI tool schema built from the task's params; invoked via `dicode.run_task()` |
| **Built-in tool** | A dicode SDK operation the agent can perform directly | `permissions.dicode` in the agent's own `task.yaml` | As an OpenAI tool schema, offered only when the matching capability was granted |
| **Skill** | A markdown file with domain context | `dicode-buildin/skills/*.md` | Advertised by name and description in the system prompt; the body is fetched on demand via `dicode_read_skill` |

This mirrors the convention used by Claude Code and the broader agent ecosystem. Think of tools as *capabilities* and skills as *knowledge*.

### Tools (task-calling)

By default, the agent can call any registered task except itself. Restrict the tool list via the `tools` param — comma-separated task ids:

```bash
curl -X POST http://localhost:8080/hooks/ai \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "what failed last night?",
    "tools": "examples/weekly-report,examples/log-digest"
  }'
```

The agent still has full access to the *result* of each tool call — it's a scoping mechanism, not a permission system. Real permission control is in the task's `permissions.dicode.tasks` allowlist (the buildin uses `["*"]`; presets inherit this unless overridden).

### Built-in tools (SDK-calling)

Firing another task is not the only thing the model can do. Every capability the
agent's own `permissions.dicode` block grants is also offered as a tool, so the
model can act on dicode itself rather than only on tasks:

| Tool | Granted by |
| ---- | ---------- |
| `dicode_list_tasks` | `list_tasks` |
| `dicode_get_runs` | `get_runs` |
| `dicode_test_task` | `tasks_test` |
| `dicode_list_sources` | `sources_list` |
| `dicode_set_dev_mode` | `sources_set_dev_mode` |
| `dicode_get_run_input` | `runs_get_input` |
| `dicode_pin_run_input` / `dicode_unpin_run_input` | `runs_pin_input` / `runs_unpin_input` |
| `dicode_replay_run` | `runs_replay` |
| `dicode_git_commit_push` | `git_commit_push` |

The grant is the whole gate: a capability the taskset never declared produces no
tool, so the model never sees an operation it would only be denied on calling.
The generic `ai-agent` buildin declares `list_tasks` and nothing else, so
`dicode_list_tasks` is the one built-in every preset inherits; the rest arrive
only where a taskset asks for them. The authoring presets (`task-create`,
`auto-fix`) declare the set their write → test → land loop needs.

Some arguments behind these tools are withheld from the model on purpose:

- `dicode_set_dev_mode` takes no `run_id` — the clone is named after the calling
  run, so two sessions cannot reach into each other's working copy — and no
  `local_path`, which would let the caller choose what the daemon loads as
  tasks. It returns `clone_path`, which is what `git-pr` expects.
- `dicode_list_sources` returns no host paths. Finding a source to work in and
  learning the daemon's filesystem layout are different needs, and only the
  first one is served here; `dicode_set_dev_mode` hands back the path to the
  clone it just made.
- `dicode_git_commit_push` takes no `allow_main`, and reads its branch prefix
  from the `git_branch_prefix` param. A prefix the model picked alongside the
  branch would bound nothing.

### Skills (prompt markdown)

Drop a file into `dicode-buildin/skills/` and reference it by name without the extension:

```bash
curl -X POST http://localhost:8080/hooks/ai \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "review the overnight deploys",
    "skills": "dicode-basics,deploy-runbook"
  }'
```

Every name you pass is read at the start of the run. What reaches the model is decided by `skills_mode`:

| `skills_mode` | What the system prompt carries | Lookup tool |
| ------------- | ------------------------------ | ----------- |
| `index` (default) | One line per skill: its name and its frontmatter `description` | `dicode_read_skill` returns a skill's full body |
| `eager` | Every skill's full body, on every turn | not offered |

Prefer `index`. A skill's full text is many times the size of the agent's own `system_prompt`, and a model reading both tends to imitate the skill's examples rather than follow its instructions — measured on an 8B local model, eager-loading 22 KB of skill took a correct task manifest from 8/8 to 0/8 while leaving the tool-call protocol untouched. The cost is also paid on every iteration of the tool loop, since the system prompt is rebuilt each time. Reach for `eager` only when the model cannot be relied on to call a tool before it acts.

Under `index`, write your `system_prompt` so it names the skill to read and when — the description alone does not tell the model what the skill will say.

The two modes also differ in where the block sits. The index goes **before** your `system_prompt`, so your instructions are the last thing the model reads before the request; the same index placed after it took structured tool calls from 6/6 to 0/6 on an 8B model, which narrated the plan it had just been told to follow instead of executing it. Eager bodies stay **after** your `system_prompt`, where they have always been, so opting back into `eager` reproduces the prompt it produced before.

Missing or unreadable skills still appear in the index, carrying `(not loaded: …)`, and `dicode_read_skill` returns the same. The reason is deliberately coarse — `no such skill file` or `unreadable` — because it reaches the model; the full error and the path it tried go to the run log.

The shared skills directory is configured through the `skills_dir` param, whose default is `${TASK_SET_DIR}/../skills` — expanded at task-load time to a sibling `skills/` directory next to the taskset that loaded the ai-agent. Override per-run to point at a different pool. See [../task-template-vars.md](../task-template-vars.md) for the full list of template variables available in task.yaml.

A starter skill ships at `dicode-buildin/skills/dicode-basics.md` covering core dicode concepts an agent should know to be useful.

---

## Picking the task the WebUI and CLI use

The WebUI's in-task AI chat panel and the `dicode ai` CLI both forward to a
single configurable task, named by `ai.task` in `dicode.yaml`:

```yaml
ai:
  task: buildin/dicodai   # default — change to any ai-agent preset
```

When omitted the default is `buildin/dicodai`, a preset of `buildin/ai-agent`
preloaded with the `dicode-task-dev` skill. Point `ai.task` at any preset
(e.g. `examples/ai-agent-ollama`) to swap providers, skills, or model without
changing code.

Two surfaces read this setting:

- `POST /api/ai/chat` — used by the WebUI chat panel. Forwards the JSON body
  to the configured task's webhook and returns its response. Requires a valid
  dicode session (gated by `requireAuth` when `server.auth: true`).
- `dicode ai "<prompt>" [--session-id ID] [--task TASK_ID]` — fires the
  configured task through the engine over the CLI control socket. Use
  `--task` to override for a single invocation; use `--session-id` to continue
  an existing conversation. The first turn's generated session id is printed
  to stderr as `session: <id>` so it doesn't pollute reply-consuming pipes.
  A failed turn (e.g. `not_configured`) prints the run's own `reply`/`error`/`hint`
  detail alongside the run id, not just a bare "finished with status failure" —
  the CLI reads it off the same structured output a webhook caller gets.

---

## Provider presets

The buildin ships **maximally restrictive**: empty `permissions.net`, no provider env vars, no defaults for `model` / `base_url` / `api_key_env`. On its own, hitting `/hooks/ai` fails the run with a `not_configured` envelope (see above). This keeps the buildin generic and safe — provider-specific policy (which hosts to reach, which env vars to read) lives with the provider-specific task.

Three ready-to-use presets live in `tasks/examples/taskset.yaml`:

| Preset | Webhook | Model | Notes |
| ------ | ------- | ----- | ----- |
| `ai-agent-ollama` | `/hooks/ai/ollama` | `llama3.2` | Local via `localhost:11434`. No key needed. |
| `ai-agent-openai` | `/hooks/ai/openai` | `gpt-4o-mini` | Needs `OPENAI_API_KEY` in the daemon env. |
| `ai-agent-groq` | `/hooks/ai/groq` | `llama-3.3-70b-versatile` | Needs `GROQ_API_KEY`. Free tier is generous. |

> **Authenticated Ollama proxies**: the `ai-agent-ollama` preset omits `api_key_env`, so the agent sends the literal string `unused` as its API key (Ollama itself ignores it, the OpenAI SDK just needs something non-empty). If you front Ollama with an *authenticated* reverse proxy, override `api_key_env` in your own taskset — otherwise the agent sends `unused` instead of your key and the proxy returns 401.

Each preset reuses the same `task.ts` via taskset `overrides` — zero code duplication. The override pattern is reusable: copy an existing preset and swap `model` / `base_url` / `api_key_env` / `permissions.net` to point at whatever provider you want.

```yaml
ai-agent-together:
  ref:
    path: ../buildin/ai-agent/task.yaml
  overrides:
    trigger:
      webhook: /hooks/ai/together
    params:
      model: "meta-llama/Llama-3.3-70B-Instruct-Turbo"
      base_url: "https://api.together.xyz/v1"
      api_key_env: "TOGETHER_API_KEY"
    env:
      - TOGETHER_API_KEY
    net:
      - api.together.xyz
```

Per-request overrides work too — just pass `model` / `base_url` / `api_key_env` as params. Useful for experimenting with different models against the same webhook.

---

## Security notes

- **The agent webhook requires authentication.** Both `buildin/ai-agent` (`/hooks/ai`) and `buildin/ai-agent-claude-cli` (`/hooks/ai-claude`) set `trigger.auth: true`. When `server.auth: true` is configured in `dicode.yaml`, callers must present a valid session cookie; anonymous POSTs return 401. The `/api/ai/chat` proxy path additionally accepts a Bearer API key. **`trigger.auth: true` is only meaningful when `server.auth: true` is set** — without it the daemon runs in unauthenticated local-dev mode and the per-webhook gate cannot enforce credentials. Task `permissions.dicode.tasks: ["*"]` is scoped behind that auth gate.
- **API keys never reach the model.** Credentials are resolved from `Deno.env.get(api_key_env)` at task start and used only to construct the OpenAI client. They are never included in the conversation history, never returned to the caller, and never logged.
- **Model output is rendered as `textContent` in the chat UI**, never `innerHTML`. Tool call arguments are also string-stringified before being passed to `dicode.run_task()`, which itself receives the already-whitelisted task id map.
- **The session KV is per-task and isolated.** Session blobs are stored under the `buildin/ai-agent` task namespace; they are not visible to any other task unless you explicitly grant cross-task KV access (which dicode does not support today).

---

## Follow-up work

The v1 buildin is deliberately minimal. Known follow-ups tracked separately:

- **Streaming tokens** — the webui run-log is already WebSocket-broadcast, so a streaming chat UI is a clean additive change. Not in v1.
- **History rehydration on reload** — users keep `session_id` in localStorage but the DOM is blank after reload. Proper rehydration needs a way for the browser to read the task's KV.
- **CLI chat** — `dicode chat [preset]` as a REPL, persists `session_id` in a dotfile.
- **Suspendable tasks (shipped)** — `dicode.suspend()` (both SDKs) lets a task pause and request input before continuing; the chat flow is a natural fit for the same suspend/resume cycle. See [Suspendable tasks](suspendable-tasks.md).
- **Zero-paste OAuth onboarding (shipped)** — the `ai-agent-openrouter` preset chains [`auth/openrouter-oauth`](../oauth.md) via the `if_missing` directive on its API-key env entry. First run with no key stored → engine fires the OAuth task, user clicks the authorize link, key is stored, retries just work. Uses OpenRouter's `callback_url`-as-request-param PKCE flow, so no relay broker is needed. See [task-format § permissions.env](../concepts/task-format.md#permissionsenv--environment-variables) for the general `if_missing` mechanism.
