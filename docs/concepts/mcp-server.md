# MCP Server

Dicode exposes its core operations as [MCP (Model Context Protocol)](https://modelcontextprotocol.io) tools. Any MCP-capable AI agent — Claude Code, Cursor, a custom agent — can develop, test, and deploy tasks without human intervention.

---

## Enabling the MCP server

The MCP server runs in the same process as the WebUI. It is enabled by default.

```yaml
server:
  mcp: true    # default: true
  port: 8080
```

The MCP server is mounted at `http://localhost:8080/mcp`.

**Claude Code:** add dicode as an MCP server:
```bash
claude mcp add dicode http://localhost:8080/mcp
```

Or, if dicode is running locally:
```json
// .claude/mcp.json
{
  "mcpServers": {
    "dicode": {
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

---

## Tools

### `list_tasks`

Returns all registered tasks with their ID, name, trigger type, last run status, and last run time.

```json
{
  "tasks": [
    {
      "id": "morning-email-check",
      "name": "Morning Email Check",
      "trigger": "cron: 0 8 * * *",
      "status": "success",
      "last_run": "2026-03-22T08:00:01Z"
    }
  ]
}
```

### `get_task(id)`

Returns the full task: `task.yaml`, `task.js`, and `task.test.js` content.

### `get_js_api`

Returns the full JS globals reference. Use this to understand what globals are available before writing a task.

### `get_example_tasks`

Returns 2–3 curated example tasks (task.yaml + task.js + task.test.js). Use for few-shot context when generating a new task.

### `list_secrets`

Returns registered secret names. **Never returns values.**

```json
{ "secrets": ["SLACK_TOKEN", "GMAIL_TOKEN", "OPENAI_API_KEY"] }
```

### `write_task_file(path, content)`

Write a file into the local development source directory.

```json
{
  "path": "morning-email-check/task.js",
  "content": "const res = await http.get(...)"
}
```

Equivalent to saving a file in your editor — the local source picks it up via fsnotify within ~100ms.

### `validate_task(id_or_path)`

Run static validation (Layer 1) on the task. Returns structured errors.

```json
{
  "valid": false,
  "errors": [
    { "type": "yaml", "message": "trigger: cron expression invalid: '0 8 * *'" }
  ],
  "warnings": [
    { "type": "secrets", "message": "SLACK_TOKEN not found in any provider" }
  ]
}
```

### `test_task(id_or_path)`

Run `task.test.js` with mocked globals (Layer 2). Returns pass/fail per test case.

```json
{
  "passed": 2,
  "failed": 1,
  "cases": [
    { "name": "sends digest on new emails", "status": "pass" },
    { "name": "handles empty inbox", "status": "pass" },
    {
      "name": "handles auth failure",
      "status": "fail",
      "error": "AssertionError: expected http called POST https://slack.com/..."
    }
  ]
}
```

### `dry_run_task(id)`

Execute the task with real secrets but intercepted HTTP (Layer 3). Returns the execution log and return value.

```json
{
  "status": "success",
  "return_value": { "count": 5 },
  "log": [
    { "level": "info", "msg": "Found 5 messages" },
    { "level": "http_intercepted", "method": "POST", "url": "https://slack.com/..." }
  ]
}
```

### `run_task(id, params?)`

Live manual trigger. Returns a run ID that can be polled for status.

```json
{ "run_id": "run_abc123" }
```

### `get_run_log(run_id)`

Returns the execution log for a run (completed or in-progress).

### `commit_task(id, source_id)`

Promote a task from the local dev source to a git source. Creates a commit and pushes.

```json
{
  "commit_sha": "a1b2c3d4",
  "message": "Add morning-email-check task"
}
```

---

## Agent workflow

The recommended workflow for an AI agent developing a task:

```
1. list_tasks          — understand existing tasks and naming conventions
2. list_secrets        — know what secrets are available
3. get_js_api          — understand the globals before writing code
4. get_example_tasks   — few-shot context
5. write_task_file     — write task.yaml
6. write_task_file     — write task.js
7. write_task_file     — write task.test.js
8. validate_task       — fix any schema/syntax errors
9. test_task           — fix any test failures
10. dry_run_task       — verify secret resolution and HTTP targets
11. commit_task        — promote to git repo
```

**Hard rules for agents:**
- Never commit if `validate_task` or `test_task` fail
- Always write `task.test.js` alongside `task.js`
- Return value must be JSON-serializable
- Never hardcode secrets — use `env.get()` and ensure the key is in `task.yaml`
- If `validate_task` fails, fix and re-validate before proceeding to `test_task`

See [Agent Skill](./agent-skill.md) for the embeddable skill document that teaches these rules to any agent.

---

## Security

The MCP server runs on localhost by default and is not exposed to the internet. If you're running dicode on a remote server and want AI agent access, proxy it behind authentication.

There is no MCP-specific authentication in the MVP. All MCP tools operate with the same permissions as the local dicode process.
