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

The filter is a boundary, not an oversight, and it stays: `/mcp` is reachable by any API-key holder, so an unfiltered `list_tasks` would hand every caller the whole registry. An agent authoring a task does not read the registry to do it — it gets the `task.yaml` schema from its skills, and it reads and writes its own task's files inside a dev-mode clone. A task that genuinely wants to be an agent's building block opts in.

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

Returns the configured sources, sorted by name.

```json
[
  { "name": "buildin", "type": "taskset", "dev_mode": false },
  { "name": "infra", "type": "taskset", "url": "https://github.com/acme/tasks.git", "branch": "main", "dev_mode": true }
]
```

Host paths are withheld, and any userinfo in a source URL is stripped — operators routinely embed a PAT there, and the listing names the repo without handing over the credential that reaches it. `permissions.dicode.sources_list` is grantable on its own, and a caller holding only it must not learn the daemon's filesystem layout — `switch_dev_mode` is what hands back a path, and only for the clone it just created. `GET /api/sources` still carries the full record for the dashboard.

### `switch_dev_mode(source, enabled, branch?, base?, run_id?)`

Enter or leave dev mode on a TaskSet source. Entering with a `branch` clones the source repo into a scratch directory and returns `clone_path` — edit files there, not in the live source. Leaving removes the clone locally and keeps the remote branch. Changes take effect immediately: tasks from the clone appear in the registry within seconds.

```json
{ "ok": true, "dev_root_path": "/data/dev-clones/infra/run-7/taskset.yaml", "clone_path": "/data/dev-clones/infra/run-7" }
```

`local_path` is deliberately not a tool argument. It redirects the daemon's taskset resolution at any path on the host, which would let a caller decide what the daemon loads as tasks — and taskset resolution reads a ref's `auth.token_env` from the daemon environment before the approval gate is in the path (#740). Operators who need local-path dev mode use `PATCH /api/sources/{name}/dev`.

`run_id` names the per-session clone directory. A caller holding an ephemeral per-run token does not choose it: the `/mcp` forwarder overwrites whatever the call carries with the run the token was minted for, so one session cannot address another session's clone.

### `test_task(id)`

Runs the task's sibling test file (`task.test.ts` / `task.test.py`) and returns the result.

```json
{ "taskID": "infra/deploy-backend", "runtime": "deno", "passed": 3, "failed": 0, "exitCode": 0, "output": "…" }
```

Refused for a task the approval gate is still holding pending — the test file runs with full host permissions, so a pending task is turned away here exactly as it would be on a fire.

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

The `/mcp` endpoint requires a `dck_` API key (Bearer) when `server.auth: true` (`pkg/webui/apikeys.go` `requireAPIKey`, wired directly on the `/mcp` route — no session-cookie fallback). Without auth it is open on localhost. See [security.md](security.md) Phase 4 for the full authentication model.

### Ephemeral per-run tokens are capability-scoped

A task that declares `permissions.env: [{name: DICODE_MCP_API_KEY}]` gets a fresh Bearer key minted at run start and revoked at run end, instead of a static secret — see `tasks/buildin/ai-agent-claude-cli` for an example consumer. That key's MCP tool surface is scoped 1:1 to the task's own declared `permissions.dicode`:

- `list_tasks` / `get_task` require `permissions.dicode.list_tasks: true`.
- `run_task` requires the target task ID to appear in `permissions.dicode.tasks` (or `["*"]` for any task).
- `list_sources` requires `permissions.dicode.sources_list: true`.
- `switch_dev_mode` requires `permissions.dicode.sources_set_dev_mode: true`, and additionally requires the token to carry the run it was minted for — a token without one is refused rather than allowed to supply its own `run_id`.
- `test_task` requires `permissions.dicode.tasks_test: true`. The REST endpoint `POST /api/tasks/{id}/test` is a separate surface reachable with the same Bearer token; it checks the same flag, so the two carry one gate between them.

A task with no `permissions.dicode` block at all gets a token that can call none of the scoped tools — `buildin/ai-agent-claude-cli` is one such task, which is why the CLI preset is chat-only until an override declares what it needs. An unrecognized tool name is denied by default, so a tool added to `tasks/buildin/mcp/task.ts` without a matching case in `mcpScopeCheck` fails closed rather than inheriting full access.

Enforcement happens in `pkg/webui`'s `/mcp` handler, before the request ever reaches the `buildin/mcp` task — the task holds the dicode permissions every tool it serves needs, so it isn't relied on to self-restrict. A denied call gets a JSON-RPC error back (HTTP 200, `error.code: -32001`) rather than being forwarded. The same handler rewrites `switch_dev_mode`'s `run_id` before forwarding; that is the one argument a scoped caller cannot choose.

Scoping the JSON-RPC tools does not, by itself, scope every REST endpoint reachable with the same Bearer token. `tasks_test` is the one such case covered today.

Operator-, CLI-, and dashboard-created API keys (`dicode mcp install`, the dashboard's Create API Key) are **unscoped** — full access, exactly as before this feature — since they aren't tied to a single task's declared permissions.
