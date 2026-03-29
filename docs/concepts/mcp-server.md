# MCP Server

Dicode exposes its core operations as [MCP (Model Context Protocol)](https://modelcontextprotocol.io) tools. Any MCP-capable AI agent — Claude Code, Cursor, a custom agent — can list tasks, trigger runs, and control dev mode.

---

## Enabling the MCP server

The MCP server runs in the same process as the WebUI. It is enabled by default.

```yaml
server:
  mcp: true    # default: true
  port: 8080
```

The MCP server is mounted at `http://localhost:8080/mcp`.

**Protocol**: JSON-RPC 2.0 over HTTP. `POST /mcp` dispatches tool calls. `GET /mcp` returns server info.

**Claude Code:** add dicode as an MCP server:
```bash
claude mcp add dicode http://localhost:8080/mcp
```

Or via `.claude/mcp.json`:
```json
{
  "mcpServers": {
    "dicode": {
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

---

## Implemented tools

### `list_tasks`

Returns all registered tasks with ID, name, trigger type, last run status, and last run time.

```json
{
  "tasks": [
    {
      "id": "infra/deploy-backend",
      "name": "Deploy Backend",
      "trigger": "manual",
      "status": "success",
      "last_run": "2026-03-29T10:00:01Z"
    }
  ]
}
```

Namespace-scoped IDs (`infra/deploy-backend`) are returned when tasks come from a TaskSet source.

### `get_task(id)`

Returns the full task spec for a given task ID.

```json
{
  "id": "infra/deploy-backend",
  "name": "Deploy Backend",
  "trigger": { "manual": true },
  "runtime": "deno"
}
```

### `run_task(id, params?)`

Manually trigger a task. Returns a run ID.

```json
{ "run_id": "run_abc123" }
```

### `list_sources`

Returns all configured sources with their type, URL/path, branch, and current dev mode state.

```json
{
  "sources": [
    {
      "name": "infra",
      "type": "local",
      "path": "/home/user/tasks/taskset.yaml",
      "dev_mode": false,
      "dev_path": ""
    }
  ]
}
```

### `switch_dev_mode(source, enabled, local_path?)`

Enable or disable dev mode for a TaskSet source. When enabled, the source immediately re-resolves using `local_path` as the root taskset.yaml.

```json
{
  "source": "infra",
  "enabled": true,
  "local_path": "/tmp/dev-tasks/taskset.yaml"
}
```

Returns the updated source entry. Changes take effect immediately — tasks from the dev path appear in the registry within seconds.

---

## Planned tools (not yet implemented)

| Tool | Description |
| --- | --- |
| `validate_task(id)` | Static validation — schema, syntax, cycle detection |
| `test_task(id)` | Run task test file with mocked globals |
| `dry_run_task(id)` | Execute with real secrets, intercepted HTTP |
| `commit_task(id, source_id)` | Promote local task to git source |
| `list_secrets` | Registered secret names (never values) |
| `write_task_file(path, content)` | Write file into local dev source directory |

---

## Security

The MCP server runs on localhost by default and is not exposed to the internet. If running dicode on a remote server and want agent access, proxy it behind authentication.

There is no MCP-specific authentication in the MVP. All MCP tools operate with the same permissions as the local dicode process.
