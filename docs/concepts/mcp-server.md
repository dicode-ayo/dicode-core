# MCP Server

Dicode exposes its core operations as [MCP (Model Context Protocol)](https://modelcontextprotocol.io) tools. Any MCP-capable AI agent — Claude Code, Cursor, a custom agent — can list tasks, trigger runs, and control dev mode.

---

## Enabling the MCP server

The `/mcp` endpoint forwards to the `buildin/mcp` dicode task (`tasks/buildin/mcp/task.ts`), which speaks JSON-RPC 2.0; the HTTP boundary is served by the WebUI process. It is enabled by default.

```yaml
server:
  mcp: true    # default: true
  port: 8080
```

The MCP server is mounted at `http://localhost:8080/mcp`.

**Protocol**: JSON-RPC 2.0 over HTTP. `POST /mcp` dispatches tool calls. `GET /mcp` returns server info.

**Claude Code:** three ways, pick whichever:

1. **Dashboard one-click** — Security → Create API Key. The success card has a "Connect to Claude Code" expander with the install command pre-filled with the new key. Copy + paste, done.

2. **`dicode mcp install`** — zero-touch: mints a fresh API key in the daemon and runs `claude mcp add` for you:

   ```bash
   dicode mcp install
   # → mints API key "mcp-dicode" in the daemon, runs:
   #     claude mcp add --transport http dicode http://localhost:8080/mcp \
   #       --header "Authorization: Bearer dck_..."

   # Variants
   dicode mcp uninstall                   # revokes the key + runs `claude mcp remove dicode`
   dicode mcp print-config                # prints the equivalent command + .claude/mcp.json
   dicode mcp install --key dck_xxx       # opt-out: use a key you already have, skip minting

   # Re-running install rotates the key (revokes the old one with the same name first).
   ```

   **Threat model.** `dicode mcp install` mints API keys via the daemon's
   control socket. The control socket is gated by a token at
   `${DATADIR}/daemon.token` (mode 0600, owned by the daemon's user).
   Anyone with read access to that file can already drive the daemon
   end-to-end (e.g. `dicode secrets set` arbitrary values), so minting
   API keys is not a new trust boundary — it's the same boundary,
   different verb. The audit log (`Info`-level on every mint and
   revoke) is the new persistent visibility into key creation.
   CLI-managed keys are namespaced under `dicode-cli/` so the
   idempotent revoke path can't sweep dashboard-created keys.

3. **Manual** — mint a key in the dashboard, then:

   ```bash
   claude mcp add --transport http dicode \
     http://localhost:8080/mcp \
     --header "Authorization: Bearer dck_<your-key-here>"

   # Verify
   claude mcp list
   claude /mcp                        # interactive: pick dicode, see tools
   ```

Or hand-edit `.claude/mcp.json`:
```json
{
  "mcpServers": {
    "dicode": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer dck_<your-key-here>"
      }
    }
  }
}
```

When `server.auth: false`, the `Authorization` header is optional (the endpoint is open). Production deployments should always run with `server.auth: true` and an API key — see [Authentication](https://dicode.com/concepts/mcp-server#authentication) on the site docs for the full setup.

---

## Implemented tools

### `list_tasks`

Returns registered tasks with ID, name, trigger type, last run status, and last run time.

**`mcp_exposed` filter:** Tasks are hidden from MCP by default. Only tasks with `mcp_exposed: true` in their `task.yaml` appear in `list_tasks` and can be invoked via `tools/call`. This is a security feature to prevent unintended exposure of internal tasks to MCP clients.

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

### `test_task(id)`

Hint-style tool: returns a text pointer telling the MCP client to call `POST /api/tasks/{id}/test` directly (with its API key) to run the task's sibling test file. It does not run the test itself.

```json
{
  "content": [
    {
      "type": "text",
      "text": "Task tests are not exposed via the dicode task SDK. Call `POST /api/tasks/infra/deploy-backend/test` directly with your API key."
    }
  ]
}
```

---

## Planned tools (not yet implemented)

| Tool | Description |
| --- | --- |
| `validate_task(id)` | Static validation — schema, syntax, cycle detection |
| `dry_run_task(id)` | Execute with real secrets, intercepted HTTP |
| `commit_task(id, source_id)` | Promote local task to git source |
| `list_secrets` | Registered secret names (never values) |
| `write_task_file(path, content)` | Write file into local dev source directory |

---

## Security

The `/mcp` endpoint requires a `dck_` API key (Bearer) when `server.auth: true` (`pkg/webui/apikeys.go` `requireAPIKey`, wired via `requireSessionOrAPIKey`). Without auth it is open on localhost. See [security.md](security.md) Phase 4 for the full authentication model.
