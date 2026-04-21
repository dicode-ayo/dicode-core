# AI Agent

The **ai-agent** built-in task gives dicode a full chat interface backed by any OpenAI-compatible model. The agent can call your other dicode tasks as tools, persist conversations across turns, and load markdown "skills" into its system prompt for domain context.

Unlike the existing "AI generates code" feature (see [docs/concepts/ai-generation.md](ai-generation.md)), which uses an AI model to *write* tasks, ai-agent uses an AI model to *orchestrate* tasks you already have. The two features complement each other: one is about authoring, one is about operation.

---

## What you get

- **A chat page** at `/hooks/ai` (plus per-provider presets at `/hooks/ai/ollama`, `/hooks/ai/openai`, `/hooks/ai/groq`). Send a message, get a reply. Sessions persist across turns.
- **Tool use** — the model sees every registered task as a callable tool. Ask "check my weekly-report runs from last month" and the agent calls `dicode.get_runs("weekly-report")` via the corresponding task and answers based on the actual data, not a hallucination.
- **Skills** — drop markdown files into `tasks/skills/` and pass their names via the `skills` param to inject them into the system prompt. Use them to give the agent durable context it should know about every time (domain glossary, team conventions, current priorities).
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

When the buildin is called without a configured provider, it returns a structured error instead of throwing, so the UI can render a clear message:

```json
{
  "session_id": "e4b9f3a2-...",
  "reply": null,
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
| **Skill** | A markdown file with domain context | `tasks/skills/*.md` | Concatenated into the system prompt at the start of every turn |

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

### Skills (prompt markdown)

Drop a file into `tasks/skills/` and reference it by name without the extension:

```bash
curl -X POST http://localhost:8080/hooks/ai \
  -H 'Content-Type: application/json' \
  -d '{
    "prompt": "review the overnight deploys",
    "skills": "dicode-basics,deploy-runbook"
  }'
```

Skills are loaded eagerly — every name you pass is read and concatenated into the system prompt for the entire turn. Missing or unreadable skills produce a placeholder in the prompt instead of failing the request.

The shared skills directory is configured through the `skills_dir` param, whose default is `${TASK_SET_DIR}/../skills` — expanded at task-load time to a sibling `skills/` directory next to the taskset that loaded the ai-agent. Override per-run to point at a different pool. See [../task-template-vars.md](../task-template-vars.md) for the full list of template variables available in task.yaml.

A starter skill ships at `tasks/skills/dicode-basics.md` covering core dicode concepts an agent should know to be useful.

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

---

## Provider presets

The buildin ships **maximally restrictive**: empty `permissions.net`, no provider env vars, no defaults for `model` / `base_url` / `api_key_env`. On its own, hitting `/hooks/ai` returns `not_configured`. This keeps the buildin generic and safe — provider-specific policy (which hosts to reach, which env vars to read) lives with the provider-specific task.

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

- **The agent has `permissions.dicode.tasks: ["*"]` by default.** Any task registered with the runtime is callable. On a public or untrusted network this is a keys-to-the-kingdom endpoint — the chat page is currently unauthenticated pending [issue #96](https://github.com/dicode-ayo/dicode-core/issues/96) (return-to-origin auth UX for protected webhooks). Do **not** ship this in an alpha release without fixing #96. Local dev on localhost-only is fine.
- **API keys never reach the model.** Credentials are resolved from `Deno.env.get(api_key_env)` at task start and used only to construct the OpenAI client. They are never included in the conversation history, never returned to the caller, and never logged.
- **Model output is rendered as `textContent` in the chat UI**, never `innerHTML`. Tool call arguments are also string-stringified before being passed to `dicode.run_task()`, which itself receives the already-whitelisted task id map.
- **The session KV is per-task and isolated.** Session blobs are stored under the `buildin/ai-agent` task namespace; they are not visible to any other task unless you explicitly grant cross-task KV access (which dicode does not support today).

---

## Follow-up work

The v1 buildin is deliberately minimal. Known follow-ups tracked separately:

- **Streaming tokens** — the webui run-log is already WebSocket-broadcast, so a streaming chat UI is a clean additive change. Not in v1.
- **History rehydration on reload** — users keep `session_id` in localStorage but the DOM is blank after reload. Proper rehydration needs a way for the browser to read the task's KV.
- **CLI chat** — `dicode chat [preset]` as a REPL, persists `session_id` in a dotfile.
- **Suspendable tasks** — the chat flow collapses to a suspend/resume cycle under the primitive proposed in [issue #95](https://github.com/dicode-ayo/dicode-core/issues/95).
- **Zero-paste OAuth onboarding (shipped)** — the `ai-agent-openrouter` preset chains [`auth/openrouter-oauth`](../oauth.md) via the `if_missing` directive on its API-key env entry. First run with no key stored → engine fires the OAuth task, user clicks the authorize link, key is stored, retries just work. Uses OpenRouter's `callback_url`-as-request-param PKCE flow, so no relay broker is needed. See [task-format § permissions.env](../concepts/task-format.md#permissionsenv--environment-variables) for the general `if_missing` mechanism.
