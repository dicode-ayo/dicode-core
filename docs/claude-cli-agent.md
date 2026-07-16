# Claude.ai subscription as an LLM backend

dicode ships two AI-agent buildin tasks:

| Task | Backend | Auth | Billing |
|---|---|---|---|
| [`buildin/ai-agent`](../tasks/buildin/ai-agent/) | OpenAI-compatible HTTPS endpoint (OpenAI, Anthropic, Ollama, OpenRouter, …) | Per-task `*_API_KEY` env or secret | Per-token via the chosen provider's API |
| [`buildin/ai-agent-claude-cli`](../tasks/buildin/ai-agent-claude-cli/) | Local `claude` CLI subprocess | One-year OAuth token (`CLAUDE_CODE_OAUTH_TOKEN`) | Counts against your Claude.ai Pro/Max subscription rate windows |

If you have a Claude subscription, the CLI variant lets dicode use the same
quota you're already paying for — no per-token API charges.

## When to pick which

- **Pick `ai-agent-claude-cli` when:** you have a Claude Pro/Max
  subscription and your dicode workload fits within the 5-hour rate
  window (typical auto-fix loops, occasional ad-hoc agent calls).
- **Pick `ai-agent` when:** you need a non-Anthropic model, want
  predictable per-token cost, or your workload exceeds subscription
  rate limits.

Both can be installed alongside each other; nothing prevents one task
from using the API path and another from using the subscription path.

## How the CLI wrapper works

`ai-agent-claude-cli`'s `task.ts` is a thin Deno wrapper around `claude
-p "<prompt>" --output-format json`. The Claude CLI is the official
`@anthropic-ai/claude-code` package's binary entry point — when launched
with a `CLAUDE_CODE_OAUTH_TOKEN` env var (minted by `claude
setup-token`), it talks to Claude using your subscription credentials,
not an API key.

Sessions are server-managed: omit `session_id` for a new conversation;
the response carries a `session_id` you can pass back on subsequent
turns to continue. The wrapper itself is stateless beyond the OAuth
token cache the CLI maintains under `$HOME/.claude/`.

The interactive chat loop carries two ids across suspend/resume turns: a
dicode-minted `chatId` (keys the per-invocation workdir) and the Claude CLI's
own `claudeSessionId` (passed to `--resume`). Both are attacker-influenceable
via a crafted resume submission, so the wrapper validates each as a UUID
before use — an off-shape `chatId` gets a fresh workdir instead of reaching
`Deno.Command`'s `cwd` (defanging path traversal), and an off-shape
`claudeSessionId` is dropped instead of reaching the `--resume` subprocess
argument (defanging argument injection). The wrapper also redacts the OAuth
token from any error output forwarded back to the caller.

## Install paths

The dicode-core image is distroless — it ships with the Go binary only,
no Deno, no Claude. Choose the install path that fits your deployment:

- **Plain host install:** `curl -fsSL https://install.claude.ai | bash`
  drops the binary at `~/.local/bin/claude`. Make sure that directory is
  on the daemon's PATH.
- **Custom Docker image:** build a derivative of the dicode-core image
  with the binary copied in. See the
  [task README](../tasks/buildin/ai-agent-claude-cli/README.md#option-b--custom-docker-image-containerized-deployments)
  for a full Dockerfile.
- **Kubernetes init container:** mount an emptyDir between an Alpine
  init container that runs the Claude installer and the main dicode
  container. See the [task README](../tasks/buildin/ai-agent-claude-cli/README.md#option-c--kubernetes-init-container-no-image-rebuild).

Once installed, drop the OAuth token into dicode's secrets store as
`CLAUDE_CODE_OAUTH_TOKEN` (via the WebUI or
`dicode secrets set CLAUDE_CODE_OAUTH_TOKEN <value>`).

## Using with the auto-fix loop

The `buildin/auto-fix` preset's `ai_task` param selects which agent task
drives the loop. To switch from the default OpenAI-compatible
`buildin/ai-agent` to the Claude-CLI variant, declare a sibling override
entry in your taskset:

```yaml
auto-fix-claude:
  ref:
    path: ./auto-fix/task.yaml
  overrides:
    params:
      ai_task: "ai-agent-claude-cli"
    dicode:
      tasks: ["ai-agent-claude-cli", "git-pr"]
```

Then point `on_failure_chain` at `buildin/auto-fix-claude` instead of
`buildin/auto-fix`.

## Limitations (current)

- **No tool-use beyond `claude -p` defaults.** Print mode disables
  Claude's filesystem / bash tools. For deeper agentic loops, drive the
  CLI's session machinery from outside dicode.
- **No streaming.** The wrapper waits for the full response. The CLI
  supports `--output-format stream-json`; plumbing that through
  `dicode.output()` for live tokens is a future enhancement.
- **No subscription-aware queueing.** Hitting the 5-hour window returns
  an error; the task has no built-in retry or backpressure.
- **Subscription tokens are one user.** OAuth tokens belong to one
  Claude account. If you operate dicode for a team, every operator
  shares one quota; the API path may scale better for multi-tenant
  setups.
