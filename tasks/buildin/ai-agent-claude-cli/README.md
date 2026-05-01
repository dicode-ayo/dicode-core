# buildin/ai-agent-claude-cli

Wraps the official `claude` CLI so dicode tasks can drive Claude with the
operator's **Claude.ai Pro/Max subscription** instead of paying per-token via
the Anthropic API.

Companion to [`buildin/ai-agent`](../ai-agent/) (OpenAI-compatible HTTPS
endpoints, per-token API key billing). Pick one:

| | `buildin/ai-agent` | `buildin/ai-agent-claude-cli` |
|---|---|---|
| Backend | Any OpenAI-compatible HTTP endpoint | Claude CLI subprocess |
| Auth | Per-task `*_API_KEY` env / secret | One-year OAuth token (`CLAUDE_CODE_OAUTH_TOKEN`) |
| Billing | Per-token via API | Counts against subscription rate windows (Pro/Max: 5-hour) |
| Tools | dicode tasks (via `--tools` param) | Claude's own internal tools (when not in `-p` mode) |
| Setup | Provide an API key | Mint OAuth token + install `claude` binary |

## Setup

> **Run with `server.auth: true`.** The webhook (`/hooks/ai-claude`)
> is unauthenticated by default — pending [#96](https://github.com/dicode-ayo/dicode-core/issues/96)
> for per-task webhook auth. Until that lands, anyone who can reach the
> daemon's port can invoke the operator's Claude subscription quota.
> Setting `server.auth: true` in `dicode.yaml` gates the whole HTTP
> surface behind the session-cookie + API-key auth chain, which is the
> recommended posture for any deployment beyond a personal laptop.

### 1. Mint a Claude OAuth token

On any machine where you've signed into Claude Code with your Pro/Max account:

```sh
claude setup-token
```

This emits a one-year token. Store it in dicode's secrets store under the key
`CLAUDE_CODE_OAUTH_TOKEN` (via the WebUI or `dicode secrets set`).

### 2. Install the `claude` binary on the daemon host

The buildin task assumes `claude` is reachable — either on `PATH` or via the
`CLAUDE_CLI_PATH` env var (set in `dicode.yaml` `defaults.env` so all tasks
inherit it). Pick the install path that matches your deployment:

#### Option A — Plain host install (laptops, single VMs)

```sh
curl -fsSL https://install.claude.ai | bash
```

Anthropic's official installer detects platform, downloads the native binary,
drops it at `~/.local/bin/claude`. Make sure that's on the daemon's `PATH`.

#### Option B — Custom Docker image (containerized deployments)

The published `dicode-core` image is distroless and intentionally minimal.
For container deployments, build a derivative image that adds Claude:

```dockerfile
# Dockerfile.with-claude
FROM ghcr.io/dicode-ayo/dicode-core:latest

# Switch to root briefly to install + chown
USER root
RUN apk add --no-cache curl libgcc libstdc++ \
    && curl -fsSL https://install.claude.ai | bash \
    && mv /root/.local/bin/claude /usr/local/bin/claude \
    && chmod +x /usr/local/bin/claude
USER nonroot
```

(Alpine-based example; adjust for the actual runtime base if you've
re-tagged. The distroless `nonroot` image has no shell — you can't `apk add`
inside it. Easiest path: build on top of `golang:alpine` or
`debian:slim` and copy the dicode binary in.)

Pin the binary version explicitly if you want stability:

```dockerfile
RUN curl -fsSL "https://install.claude.ai?version=2.1.123" | bash
```

#### Option C — Kubernetes init container (no image rebuild)

Mount a shared `emptyDir` between an init container that installs Claude
and the main dicode container that uses it:

```yaml
# Pod spec excerpt
spec:
  volumes:
    - name: claude-bin
      emptyDir: {}
  initContainers:
    - name: install-claude
      image: alpine:3.21
      command: ["/bin/sh", "-c"]
      args:
        - |
          apk add --no-cache curl &&
          curl -fsSL https://install.claude.ai | bash &&
          cp /root/.local/bin/claude /shared/claude &&
          chmod +x /shared/claude
      volumeMounts:
        - { name: claude-bin, mountPath: /shared }
  containers:
    - name: dicode
      image: ghcr.io/dicode-ayo/dicode-core:latest
      env:
        - name: CLAUDE_CLI_PATH
          value: /shared/claude
      volumeMounts:
        - { name: claude-bin, mountPath: /shared, readOnly: true }
```

### 3. Verify

Once the secret is set and the binary is reachable, fire the task:

```sh
curl -fsSL -X POST http://localhost:8080/hooks/ai-claude \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"In one sentence, what is dicode?"}'
```

The response includes a `session_id` you can pass back on the next call:

```sh
curl -fsSL -X POST http://localhost:8080/hooks/ai-claude \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"And how does it work?","session_id":"sess-..."}'
```

## Params

| Param | Type | Default | Description |
|---|---|---|---|
| `prompt` | string | required | User message. |
| `session_id` | string | `""` | Continue an earlier conversation. Empty = new. |
| `model` | string | `""` | E.g. `sonnet`, `opus`. Empty = CLI default. |
| `system_prompt` | string | `""` | Appended via `--append-system-prompt`. |
| `cli_path` | string | `""` | Override binary path. Empty = `CLAUDE_CLI_PATH` env, then `PATH` lookup. |
| `skills` | string | `""` | Comma-separated skill markdown filenames (without `.md`). Each is copied from `skills_dir` into a per-invocation `.claude/skills/` that the Claude CLI loads automatically. |
| `skills_dir` | string | `${TASK_SET_DIR}/../skills` | Where the skill md files live. |
| `enable_mcp` | bool | `true` | Wire dicode's `/mcp` endpoint into a per-invocation `.claude/mcp.json` so Claude can call dicode tasks as MCP tools. Requires `DICODE_MCP_API_KEY` (populated by `dicode mcp install`). |
| `mcp_url` | string | `http://localhost:8080/mcp` | dicode MCP endpoint URL written into `.claude/mcp.json`. |

### Skills + MCP wiring

On every invocation, the task creates `${DICODE_DATA_DIR}/tmp/claude-cli/<uuid>/.claude/`,
populates it with skill markdowns + `mcp.json`, and runs `claude -p` with that
as its CWD. Claude CLI honors `.claude/` for project-local config, so the
skills become loadable instructions and the MCP server becomes a tool surface
the agent can call.

Cleanup is **out-of-band**: `buildin/temp-cleanup` sweeps any leaf directory
under `${DICODE_DATA_DIR}/tmp/<task>/<uuid>/` that's older than 1 hour, every
10 minutes via cron. The agent task itself doesn't try/finally-clean — the
cron is the source of truth and avoids redundant work on the hot path.

The `DICODE_MCP_API_KEY` secret is auto-populated by `dicode mcp install` —
the same key the operator's local Claude Code uses, so one install command
wires both consumers. Run `dicode mcp install` once after first daemon
startup; the agent task picks up the secret without further setup.

## Output

```jsonc
{
  "ok": true,
  "reply": "...",                    // model's text
  "session_id": "<uuid>",            // dicode-side id, pass back for continuation
  "model": "claude-sonnet-4",
  "total_cost_usd": 0.0023,          // cumulative across the conversation; subscription users get 0
  "usage": { /* raw CLI usage object */ }
}
```

On failure (`ok: false`) the `error` field carries the reason. The OAuth token
is redacted before being included in any error string, even if the underlying
CLI ever logs it.

### Session-id mapping

The `session_id` in the response is a **dicode-side UUID**, not the Claude CLI's
internal id. The task stores `kv["claude:<dicode_uuid>"] = <claude_cli_session_id>`
and resolves it via `--resume` on subsequent calls. Mirrors `buildin/ai-agent`'s
session shape so the same chat UI shape works for both presets, and decouples
the wire format from any future change in Claude's session-id format.

## Rate limits

Calls count against the same 5-hour rate windows as interactive Claude Code
use. There's no per-token charge but exceeding the subscription quota returns
an error you'll see propagated back as `ok: false`.

## Using with the auto-fix loop

To swap the auto-fix preset's LLM backend from the OpenAI-compatible
`buildin/ai-agent` to this Claude-CLI variant, declare an override entry in
your taskset:

```yaml
# tasks/buildin/taskset.yaml (or your own taskset)
auto-fix-claude:
  ref:
    path: ./auto-fix/task.yaml      # the existing buildin auto-fix preset
  overrides:
    params:
      ai_task: "ai-agent-claude-cli"
    dicode:
      tasks: ["ai-agent-claude-cli", "git-pr"]
```

Then point `on_failure_chain` at `buildin/auto-fix-claude` instead of
`buildin/auto-fix`. (The auto-fix preset accepts an `ai_task` param that
selects which agent task drives the loop; if it doesn't yet, this is a small
follow-up to the auto-fix override.)

## Limitations

- **No tool-use beyond what `claude -p` supports.** This wrapper invokes the
  CLI in print mode, which doesn't enable Claude's filesystem / bash tools.
  For tool-using agentic loops, drive Claude's own session machinery from
  outside dicode (or wait for an SDK that exposes it cleanly).
- **No streaming today.** The wrapper waits for the full response before
  returning. Claude's `--output-format stream-json` is supported by the CLI;
  follow-up could plumb that through `dicode.output()` for live tokens.
- **No subscription rate-limit awareness.** When you hit the 5-hour cap, the
  call returns an error and you wait. No queuing, no automatic retry.
