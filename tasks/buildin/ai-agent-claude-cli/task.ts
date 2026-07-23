// deno-lint-ignore-file no-explicit-any
//
// buildin/ai-agent-claude-cli — wraps `claude -p` to call Claude with the
// operator's Pro/Max subscription instead of an Anthropic API key.
//
// Two shapes over one Claude-invocation core (runClaudeTurn):
//   - One-shot (a non-empty `prompt` param): run one stateless Claude turn and
//     return { ok, reply, session_id, model, ... }. Each call is a fresh Claude
//     session — no --resume, no cross-call state. The bare-return shape matches
//     buildin/ai-agent so the same chat UI works in both presets.
//   - Chat loop (no `prompt` on a fresh run): suspend to a `turn` step that
//     collects a message, runs one turn, and suspends back to itself — a
//     terminal chat via `dicode run` with no CLI changes. Claude's own session
//     carries the conversation, so the suspend `state` just holds the CLI
//     session id (plus a stable chat id keying the cwd that --resume needs).
//     A blank message ends it.
//
// Logging: console.error / console.log are captured by the runtime as
// the run's stderr/stdout log entries. Error returns log to console.error
// so operators can see what went wrong even when the run exits 0 (returns
// a value, not throws).

import type { Dicode, DicodeSdk } from "../../sdk.ts";
import { chatStart, chatTurn, decideEntryMode, isChatEnd, isValidSessionId, resolveSessionId, SAFE_SKILL_NAME } from "../ai-agent-core/chat.ts";

// Re-exported so the task's tests (and any importer) reach the shared envelope
// helpers through the task module.
export { decideEntryMode, isChatEnd };

interface Params {
    get: (key: string) => Promise<string | null>;
    all: () => Promise<Record<string, string>>;
}

interface ClaudeResponse {
    type: string;
    subtype?: string;
    is_error?: boolean;
    result?: string;
    session_id?: string;
    model?: string;
    total_cost_usd?: number;
    usage?: unknown;
    [k: string]: unknown;
}

interface SDK {
    params: Params;
    dicode: Dicode;
}

// The outcome of one Claude turn. Success carries the reply and the CLI's new
// session id (the caller decides where to persist it); failure carries a
// redacted error already logged via fail().
type TurnResult =
    | { ok: true; reply: string; claudeSessionId: string; model: string; total_cost_usd: number; usage: unknown }
    | { ok: false; error: string };

function fail(error: string): { ok: false; error: string } {
    // Surface in the run log as well — return-value-only errors are
    // invisible in the dashboard otherwise (the run exits 0 because we
    // returned a value rather than throwing).
    console.error("ai-agent-claude-cli: " + error);
    return { ok: false, error };
}

// Claude's built-in filesystem/exec/network/agent tools — never dicode's
// mcp__dicode tools, which are the intended capability surface. Deny-listed
// unconditionally below regardless of MCP wiring state; see the comment at
// the call site. Covers every built-in tool that can read/write the host fs,
// run subprocesses, reach the network, or spawn a sub-agent that could reach
// any of those transitively (Task) — not just the most obviously dangerous
// four (Bash/Read/Write/Edit).
const DANGEROUS_BUILTIN_TOOLS = [
    "Bash", "Read", "Write", "Edit", "NotebookEdit", "WebFetch", "WebSearch",
    "Glob", "Grep", "Task", "KillShell",
];

/**
 * Build the `claude` CLI argument vector. Exported for unit testing.
 *
 * MCP: `<cwd>/.claude/mcp.json` is NOT an auto-load path for the Claude CLI
 * (the auto-loaded project file is `.mcp.json` at the root, and even that hits a
 * headless approval gate). So when a config was written we mount it explicitly
 * with `--mcp-config`, and `--strict-mcp-config` loads ONLY that server —
 * ignoring the operator's `~/.claude.json` / project `.mcp.json`. Passing
 * neither flag (the prior behavior) meant Claude ran with zero dicode tools.
 */
export function buildClaudeArgs(opts: {
    prompt: string;
    claudeSessionId?: string;
    model?: string;
    systemPrompt?: string;
    mcpConfigPath?: string;
}): string[] {
    const args = ["-p", opts.prompt, "--output-format", "json"];
    if (opts.claudeSessionId) args.push("--resume", opts.claudeSessionId);
    if (opts.model)           args.push("--model", opts.model);
    if (opts.systemPrompt)    args.push("--append-system-prompt", opts.systemPrompt);
    if (opts.mcpConfigPath) {
        args.push("--strict-mcp-config", "--mcp-config", opts.mcpConfigPath);
        // Auto-approve the dicode MCP tools so the agent can call them
        // non-interactively — `claude -p` refuses un-allowlisted tools rather
        // than prompting. Scoped to the `dicode` server only, so the agent gets
        // dicode's governed tool surface and NOT raw Bash/Write/Read host access.
        args.push("--allowedTools", "mcp__dicode");
    }
    // Fail-closed (#560): deny the dangerous built-in tools unconditionally,
    // not just when MCP happens to be wired. Before this, an unset
    // mcpConfigPath (MCP disabled, or MCP wiring failed) pushed NO
    // --allowedTools/--disallowedTools at all, so Claude silently fell back to
    // its full default toolset — Bash/Read/Write/Edit/etc — with real host
    // filesystem + subprocess-exec access as the daemon's OS user,
    // non-interactively. This list must reach the CLI on every invocation,
    // independent of whether the mcp__dicode allowlist above was added.
    args.push("--disallowedTools", DANGEROUS_BUILTIN_TOOLS.join(","));
    return args;
}

export default async function main({ params, dicode }: SDK) {
    const prompt = (await params.get("prompt")) ?? "";

    if (decideEntryMode(prompt) === "chat-start") {
        // No prompt on a fresh run → open the chat loop. Claude's own session
        // carries the conversation across turns; seed it empty. chatId keys the
        // per-invocation workdir, which must stay constant across turns because
        // Claude CLI's `--resume` only finds sessions created in the same cwd.
        const chatId = crypto.randomUUID();
        await chatStart(dicode, { claudeSessionId: "", chatId }, "Message Claude — leave blank to end.");
        return; // unreachable — suspend() never returns
    }

    return await oneShotTurn({ prompt, params });
}

// The Claude-side state carried across chat turns: the CLI session id (drives
// --resume) and a stable workdir key.
interface ClaudeChatState {
    claudeSessionId?: string;
    chatId?: string;
}

export const steps = {
    // One chat turn: run Claude resuming its prior session. The envelope owns the
    // suspend/resume loop; runTurn here is just the Claude invocation, and onEnd
    // reports the CLI session id carried in state when a blank message ends it.
    turn(ctx: DicodeSdk) {
        const { params } = ctx;
        return chatTurn(
            ctx,
            async ({ message, state }) => {
                const carried = (state ?? {}) as ClaudeChatState;

                // chatId is minted in main() and carried forward; it becomes a
                // filesystem path component (the per-invocation workdir), so a
                // hand-crafted resume can't be trusted to hand back a safe value —
                // validate as a UUID and mint a fresh one (fresh cwd, no --resume)
                // rather than let an off-shape string reach Deno.Command's cwd.
                const hasValidChatId = !!carried.chatId && isValidSessionId(carried.chatId);
                const chatId = resolveSessionId(carried.chatId, "ai-agent-claude-cli: chatId", { autoMint: true });

                // claudeSessionId becomes the `claude --resume <id>` subprocess
                // argument. Same reasoning: validate before use, and treat an
                // off-shape carried value as absent (fresh Claude session) rather
                // than pass it through to the CLI invocation. Also: Claude CLI
                // sessions are cwd-scoped (--resume only finds sessions created in
                // the same workdir), so if chatId had to be replaced above, any
                // carried claudeSessionId is now orphaned for the NEW workdir too
                // — drop it rather than pass a validly-shaped but unresumable id
                // to --resume (which would fail the turn instead of gracefully
                // starting a fresh session).
                const priorClaudeSessionId = hasValidChatId
                    ? resolveSessionId(carried.claudeSessionId, "ai-agent-claude-cli: claudeSessionId", { autoMint: false })
                    : "";

                const turn = await runClaudeTurn({
                    message,
                    priorClaudeSessionId,
                    workdirKey: chatId,
                    params,
                });
                if (!turn.ok) return turn;
                return { ok: true, reply: turn.reply, state: { claudeSessionId: turn.claudeSessionId, chatId } };
            },
            (state) => ({
                ok: true,
                reply: "(chat ended)",
                // A blank message short-circuits straight to this callback — the
                // runTurn validation above never runs — so validate here too rather
                // than echo a possibly off-shape carried id back to the caller.
                session_id: resolveSessionId((state as ClaudeChatState | undefined)?.claudeSessionId, "ai-agent-claude-cli: claudeSessionId(end)", { autoMint: false }),
            }),
        );
    },
};

// oneShotTurn runs a single stateless Claude turn and returns the
// buildin/ai-agent-shaped result. Each call starts a fresh Claude session (no
// --resume); multi-turn conversation lives on the chat loop. The Claude
// invocation itself is delegated to runClaudeTurn, shared with the chat loop.
async function oneShotTurn(
    { prompt, params }: { prompt: string; params: Params },
): Promise<unknown> {
    // A fresh id keys the per-invocation workdir and is echoed back as the
    // handle; it threads no state, so a new one per call is correct.
    const sessionId = crypto.randomUUID();

    const turn = await runClaudeTurn({
        message: prompt,
        priorClaudeSessionId: "",
        workdirKey: sessionId,
        params,
    });
    if (!turn.ok) return turn;

    return {
        ok:             true,
        reply:          turn.reply,
        session_id:     sessionId,
        model:          turn.model,
        total_cost_usd: turn.total_cost_usd,
        usage:          turn.usage,
    };
}

// runClaudeTurn is the reusable Claude-invocation core: resolve auth + binary,
// build the per-invocation .claude/ workdir (mcp.json + skills), spawn
// `claude -p`, and decode the response. It is session-mapping-agnostic — the
// caller passes the prior Claude session id and the workdir key, and gets back
// the new session id to persist (or carry in suspend state) however it likes.
async function runClaudeTurn(opts: {
    message: string;
    priorClaudeSessionId: string;
    workdirKey: string;
    params: Params;
}): Promise<TurnResult> {
    const { message, priorClaudeSessionId, workdirKey, params } = opts;

    // Defense-in-depth: workdirKey becomes Deno.Command's cwd and
    // priorClaudeSessionId (when non-empty) becomes the `--resume` argument.
    // Both callers (steps.turn, oneShotTurn) already validate/self-generate
    // these before calling in, so this should never trip — but runClaudeTurn
    // is the function that actually owns the sink, so it shouldn't rely
    // entirely on caller discipline to keep an off-shape id from reaching it.
    if (!isValidSessionId(workdirKey)) {
        return fail(`internal error: workdirKey is not a valid session id — refusing to build a workdir path from it`);
    }
    if (priorClaudeSessionId && !isValidSessionId(priorClaudeSessionId)) {
        return fail(`internal error: priorClaudeSessionId is not a valid session id — refusing to pass it to --resume`);
    }

    const model         = (await params.get("model"))         ?? "";
    const systemPrompt  = (await params.get("system_prompt")) ?? "";
    const cliPathParam  = (await params.get("cli_path"))      ?? "";
    const skillsParam   = (await params.get("skills"))        ?? "";
    const skillsDir     = (await params.get("skills_dir"))    ?? "";
    const enableMcp     = ((await params.get("enable_mcp"))   ?? "true").toLowerCase() !== "false";
    const mcpURL        = (await params.get("mcp_url"))       ?? "http://localhost:8080/mcp";
    const workdirBase   = (await params.get("workdir_base"))  ?? "";

    // Dual-mode auth: prefer an explicit CLAUDE_CODE_OAUTH_TOKEN secret
    // (portable for headless / containerized daemons where no interactive login
    // exists), but if it is unset fall back to the logged-in credentials at
    // $HOME/.claude/.credentials.json — so a local daemon running as an
    // already-logged-in user needs no token. We can't see the credential file
    // from inside the Deno sandbox (the claude subprocess reads it, not us), so
    // we don't hard-fail here; a genuine no-auth situation surfaces as claude's
    // own auth error downstream.
    const oauthToken = Deno.env.get("CLAUDE_CODE_OAUTH_TOKEN") ?? "";

    const cliPath = resolveCliPath(cliPathParam);
    if (!cliPath) {
        return fail("claude binary not found. Set the cli_path param, or install via one of the paths in tasks/buildin/ai-agent-claude-cli/README.md.");
    }

    // Per-invocation working directory. Claude CLI honors a project-local
    // `.claude/` dir at the invocation cwd: `.claude/mcp.json` configures
    // additional MCP servers, `.claude/skills/*.md` are loaded as skills.
    //
    // workdirKey (dicode session id for one-shot, chat id for the loop) is
    // stable across a conversation's turns — it MUST be, because Claude CLI's
    // conversation state is cwd-scoped: a `--resume <claude_session_id>` can
    // only find sessions created in the same working directory. A fresh key per
    // turn would fail every `--resume` with "No conversation found".
    //
    // workdir_base resolves via the ${DATADIR} template variable at spec-load
    // time (see task.yaml), so we don't depend on DICODE_DATA_DIR being set in
    // the spawned env.
    //
    // Cleanup is handled out-of-band by buildin/temp-cleanup, which sweeps
    // ${DATADIR}/tmp/<task>/<key>/ leaves older than 1 hour on its 10-minute
    // cron. Active sessions keep their workdir mtime fresh (we rewrite mcp.json
    // + skills on every turn) so they're never preempted; idle sessions are
    // reaped after the TTL.
    if (!workdirBase) {
        return fail("workdir_base param is empty; expected the default ${DATADIR}/tmp/claude-cli to resolve at spec-load time");
    }
    const workdir = `${workdirBase}/${workdirKey}`;
    const claudeDir = `${workdir}/.claude`;

    // Wrap setup in a try/catch so any unexpected error (NotCapable when
    // permissions.fs is mis-scoped, ENOSPC, an immutable mount, …) comes back to
    // the caller as a structured { ok: false, error } via fail() rather than as
    // an uncaught Deno promise rejection that the runtime surfaces only in the
    // run log.
    let mcpWired = false;
    let skillsWired = 0;
    const mcpKey = Deno.env.get("DICODE_MCP_API_KEY") ?? "";
    try {
        await Deno.mkdir(`${claudeDir}/skills`, { recursive: true });

        // MCP wiring: write .claude/mcp.json so Claude can call dicode tasks as
        // MCP tools. The API key is the same one the operator's local Claude Code
        // uses (stashed by `dicode mcp install`); empty = skip and let Claude run
        // without dicode-task tool access.
        if (enableMcp && mcpKey) {
            const mcpConfig = {
                mcpServers: {
                    dicode: {
                        type: "http",
                        url:  mcpURL,
                        headers: { Authorization: `Bearer ${mcpKey}` },
                    },
                },
            };
            await Deno.writeTextFile(`${claudeDir}/mcp.json`, JSON.stringify(mcpConfig));
            mcpWired = true;
        }

        // Skills wiring: copy each named skill file from skills_dir into
        // .claude/skills/. Names are validated against a strict regex to defang
        // any attempt at traversal via "../" or absolute paths.
        if (skillsParam && skillsDir) {
            const names = skillsParam.split(",").map((s) => s.trim()).filter(Boolean);
            for (const name of names) {
                if (!SAFE_SKILL_NAME.test(name)) {
                    console.warn(`ai-agent-claude-cli: skipping skill ${JSON.stringify(name)} (invalid name)`);
                    continue;
                }
                const src = `${skillsDir}/${name}.md`;
                const dst = `${claudeDir}/skills/${name}.md`;
                try {
                    await Deno.copyFile(src, dst);
                    skillsWired++;
                } catch (e) {
                    console.warn(`ai-agent-claude-cli: skipping skill ${name}: ${e instanceof Error ? e.message : String(e)}`);
                }
            }
        }
    } catch (e) {
        return fail(`workdir setup failed at ${workdir}: ${e instanceof Error ? e.message : String(e)}`);
    }

    console.log(`ai-agent-claude-cli: invoking ${cliPath} (resume=${priorClaudeSessionId ? priorClaudeSessionId.slice(0, 8) + "…" : "no"}, model=${model || "default"}, workdir=${workdirKey.slice(0, 8)}…, mcp=${mcpWired ? "on" : "off"}, skills=${skillsWired})`);

    const args = buildClaudeArgs({
        prompt: message,
        claudeSessionId: priorClaudeSessionId,
        model,
        systemPrompt,
        // <cwd>/.claude/mcp.json is not auto-loaded; mount it explicitly and
        // strictly so only dicode's server reaches Claude.
        mcpConfigPath: mcpWired ? `${claudeDir}/mcp.json` : undefined,
    });

    const claudeEnv: Record<string, string> = {
        HOME: Deno.env.get("HOME") ?? "/root",
        PATH: Deno.env.get("PATH") ?? "/usr/local/bin:/usr/bin:/bin",
    };
    // Inject the token only when present. Setting it to "" would override the
    // $HOME/.claude credential-file fallback with an empty, invalid token.
    if (oauthToken) claudeEnv.CLAUDE_CODE_OAUTH_TOKEN = oauthToken;

    const cmd = new Deno.Command(cliPath, {
        args,
        env: claudeEnv,
        cwd: workdir,
        stdin: "null",
        stdout: "piped",
        stderr: "piped",
    });

    return await runClaudeAndDecode(cmd, oauthToken, mcpKey, model);
}

// runClaudeAndDecode is the post-setup half of runClaudeTurn: spawn claude,
// parse the JSON response, and return the structured turn result. Split out so
// the surrounding setup logic in runClaudeTurn stays readable.
async function runClaudeAndDecode(
    cmd: Deno.Command,
    oauthToken: string,
    mcpKey: string,
    fallbackModel: string,
): Promise<TurnResult> {
    let out: Deno.CommandOutput;
    try {
        out = await cmd.output();
    } catch (e) {
        return fail(`claude CLI invocation failed: ${e instanceof Error ? e.message : String(e)}`);
    }

    const stdout = new TextDecoder().decode(out.stdout).trim();
    const stderr = new TextDecoder().decode(out.stderr).trim();

    // redact strips secrets from any stream we surface to callers. Uses
    // replaceAll so multi-occurrence leaks (CLI repeats the diagnostic) are fully
    // scrubbed; String.replace would only catch the first match. Both the OAuth
    // token AND the dicode MCP API key are sensitive — strip both, even though
    // neither should ever leave their respective stores.
    const redact = (s: string) => {
        let r = s;
        if (oauthToken) r = r.replaceAll(oauthToken, "<redacted>");
        if (mcpKey)     r = r.replaceAll(mcpKey,     "<redacted>");
        return r;
    };

    if (!out.success) {
        // Surface stderr for operator debugging; don't leak the OAuth token even
        // if the CLI ever logs it (it shouldn't, but defense in depth).
        return fail(`claude exited ${out.code}: ${redact(stderr) || "(no stderr)"}`);
    }

    let parsed: ClaudeResponse;
    try {
        parsed = JSON.parse(stdout);
    } catch (_e) {
        // stdout is normally JSON; if Claude ever logs an OAuth-token-bearing
        // diagnostic to stdout instead, the redact() guard keeps it out of the
        // run log.
        return fail(`claude returned non-JSON output: ${redact(stdout.slice(0, 500))}`);
    }

    if (parsed.is_error) {
        return fail(parsed.result ?? "claude reported an error");
    }

    console.log(`ai-agent-claude-cli: ok (model=${parsed.model ?? "?"}, cost=$${parsed.total_cost_usd ?? 0})`);

    return {
        ok:              true,
        reply:           parsed.result ?? "",
        claudeSessionId: parsed.session_id ?? "",
        model:           parsed.model ?? fallbackModel,
        total_cost_usd:  parsed.total_cost_usd ?? 0,
        usage:           parsed.usage ?? null,
    };
}

// resolveCliPath returns an absolute path to the `claude` binary, or empty if
// none can be found. Order:
//   1. explicit cli_path param
//   2. CLAUDE_CLI_PATH env (set by the operator at daemon startup)
//   3. Deno.Command resolution against PATH
function resolveCliPath(cliPathParam: string): string {
    if (cliPathParam) return cliPathParam;
    const envPath = Deno.env.get("CLAUDE_CLI_PATH") ?? "";
    if (envPath) return envPath;
    // Lean on PATH resolution — Deno.Command will spawn `claude` from PATH if we
    // don't pass an absolute path. Returning the bare name is enough; the actual
    // lookup happens at exec time.
    return "claude";
}
