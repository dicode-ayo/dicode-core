# AI Task Authoring

Describe what you want automated in plain language. Dicode opens an interactive editing session backed by an AI agent task, which writes `task.yaml` / `task.ts` (+ test file) into a sandboxed dev-mode clone of the source; you review and save (or cancel) before the change lands.

---

## How it works

1. Start a session — `POST /api/task/create` scaffolds a new task's boilerplate, or `POST /api/task/edit` opens (or resumes) an AI editing session against an existing task, both driven by session bookkeeping in `pkg/webui/authoring*.go`.
2. The session is served by the agent task configured as `ai.create_task`, working inside a dev-mode clone of the task's source so edits are isolated from the live registry until saved. `ai.create_task`'s Go-level default is `buildin/task-create` (`pkg/config/config.go`), but no `buildin/task-create` task or `dicode-task-create` skill ships in this repo — point `ai.create_task` at a real `ai-agent` preset (e.g. `buildin/dicodai`) before `/api/task/edit` / `dicode task create --ai` will actually dispatch.
3. Each turn sends a prompt (and any file context) to the agent; the agent responds with proposed file changes.
4. You review the diff in the WebUI and either continue the conversation, save, or cancel.
5. `POST /api/task/save` applies the session's changes to the source; `POST /api/task/cancel` discards them and releases the dev-mode lock.

See [Web UI & API](webui-api.md#ai-authoring) for the concrete `/api/task/*` request/response shapes.

---

## Configuration

```yaml
ai:
  task: buildin/dicodai              # default — task-detail "AI" chat in the WebUI
  create_task: buildin/task-create   # Go-level default — no task exists at this
                                      # id yet; set it explicitly (e.g. buildin/dicodai)
                                      # to use /api/task/edit today
```

`ai.task` and `ai.create_task` just point at task IDs — they carry no provider credentials themselves. Provider, model, and API key live as `params` on the agent task preset itself (see `tasks/buildin/taskset.yaml`'s `dicodai` override, which wires `buildin/ai-agent` to OpenAI via `model` / `base_url` / `api_key_env` params and an `OPENAI_API_KEY` env declaration). Point `ai.task` / `ai.create_task` at your own `ai-agent` override to swap providers, models, or skills without touching Go code.

---

## MCP vs WebUI authoring

Two paths reach the same session-based authoring flow:

| | WebUI | MCP / CLI |
|---|---|---|
| Who triggers it? | Human user via the WebUI | `dicode task create --ai` / `dicode task edit`, or an MCP-connected agent |
| Confirmation? | User reviews the diff and clicks save | Caller decides when to save |
| Use case | Interactive task creation | Scripted or agent-driven task development |

The dicode MCP surface itself (`tasks/buildin/mcp/task.ts`) only exposes `list_tasks`, `get_task`, `run_task`, `list_sources`, `switch_dev_mode`, and `test_task` — an MCP agent drives task authoring by calling the same `/api/task/*` authoring endpoints directly (with its API key), not through a dedicated MCP tool.
