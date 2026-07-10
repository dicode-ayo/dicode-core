// deno-lint-ignore-file no-explicit-any
//
// buildin/ai-agent-claude-cli — wraps `claude -p` to call Claude with the
// operator's Pro/Max subscription instead of an Anthropic API key.
//
// Session-id strategy (mirrors buildin/ai-agent):
//   - Callers pass a dicode-side session_id (or omit; we generate a UUID).
//   - We store kv["claude:<dicode_uuid>"] = <claude_cli_session_id> so a
//     repeat call with the same dicode_uuid resolves to the right Claude
//     session via --resume. The CLI's session_id never leaks to clients.
//   - The bare-return shape { session_id, reply, model, ... } matches
//     buildin/ai-agent so the same chat UI shape works in both presets.
//
// Logging: console.error / console.log are captured by the runtime as
// the run's stderr/stdout log entries. Error returns log to console.error
// so operators can see what went wrong even when the run exits 0 (returns
// a value, not throws).

interface Params {
    get: (key: string) => Promise<string | null>;
    all: () => Promise<Record<string, string>>;
}

interface KV {
    get: (key: string) => Promise<unknown>;
    set: (key: string, value: unknown) => Promise<void>;
}

const SESSION_ID_RE = /^[a-zA-Z0-9_-]{1,64}$/;
const KV_PREFIX = "claude:";

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
    kv: KV;
    dicode?: any;
}

function fail(error: string): { ok: false; error: string } {
    // Surface in the run log as well — return-value-only errors are
    // invisible in the dashboard otherwise (the run exits 0 because we
    // returned a value rather than throwing).
    console.error("ai-agent-claude-cli: " + error);
    return { ok: false, error };
}

// SAFE_SKILL_NAME caps skill filenames to a flat identifier so the
// `skills` param can't traverse out of skills_dir via "../" or absolute
// paths. Mirrors the style of ValidateRunID elsewhere in the codebase.
const SAFE_SKILL_NAME = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$/;

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
    if (opts.mcpConfigPath)   args.push("--strict-mcp-config", "--mcp-config", opts.mcpConfigPath);
    return args;
}

export default async function main({ params, kv }: SDK) {
    const prompt        = (await params.get("prompt"))        ?? "";
    const sessionIdIn   = (await params.get("session_id"))    ?? "";
    const model         = (await params.get("model"))         ?? "";
    const systemPrompt  = (await params.get("system_prompt")) ?? "";
    const cliPathParam  = (await params.get("cli_path"))      ?? "";
    const skillsParam   = (await params.get("skills"))        ?? "";
    const skillsDir     = (await params.get("skills_dir"))    ?? "";
    const enableMcp     = ((await params.get("enable_mcp"))   ?? "true").toLowerCase() !== "false";
    const mcpURL        = (await params.get("mcp_url"))       ?? "http://localhost:8080/mcp";
    const workdirBase   = (await params.get("workdir_base"))  ?? "";

    if (!prompt) {
        return fail("prompt is required");
    }
    // session_id is the dicode-side handle. Reject anything not flat-id
    // shaped so a hostile caller can't forge a key that hits arbitrary
    // KV entries via the kv.get below.
    if (sessionIdIn && !SESSION_ID_RE.test(sessionIdIn)) {
        return fail(`session_id ${JSON.stringify(sessionIdIn)} contains invalid characters; expected ^[a-zA-Z0-9_-]{1,64}$`);
    }

    const oauthToken = Deno.env.get("CLAUDE_CODE_OAUTH_TOKEN") ?? "";
    if (!oauthToken) {
        return fail("CLAUDE_CODE_OAUTH_TOKEN is not set. Mint one via `claude setup-token` and store it as a dicode secret named CLAUDE_CODE_OAUTH_TOKEN. See tasks/buildin/ai-agent-claude-cli/README.md.");
    }

    const cliPath = resolveCliPath(cliPathParam);
    if (!cliPath) {
        return fail("claude binary not found. Set the cli_path param, or install via one of the paths in tasks/buildin/ai-agent-claude-cli/README.md.");
    }

    // dicode-side session id. Generate one if the caller didn't pass one
    // (matches buildin/ai-agent's UX: first turn returns a fresh uuid).
    const dicodeSessionId = sessionIdIn || crypto.randomUUID();

    // Look up the Claude-side session id for this dicode session, if any.
    // First call: kv miss → no --resume → CLI mints a new Claude session.
    //
    // Concurrency note: two browser tabs sharing the same localStorage
    // dicode_uuid will fire simultaneous `claude --resume <same-id>`
    // subprocesses. The Claude CLI's behaviour under concurrent writes
    // to the same session is undefined; the kv-set ordering at the
    // bottom of this function is non-deterministic. Same pre-existing
    // pattern as buildin/ai-agent. Mitigation if this becomes a problem:
    // a per-session lock via kv.set with a TTL'd lease, or a fresh
    // dicode_uuid per browser tab.
    let claudeSessionId = "";
    try {
        const stored = await kv.get(KV_PREFIX + dicodeSessionId);
        if (typeof stored === "string") claudeSessionId = stored;
    } catch (e) {
        // KV miss is normal; transient KV errors are non-fatal — worst
        // case the next turn starts a fresh Claude session.
        console.warn("ai-agent-claude-cli: kv.get failed: " + (e instanceof Error ? e.message : String(e)));
    }

    // Args are assembled by buildClaudeArgs() just before spawn (below), after
    // MCP setup, so the --mcp-config flags reflect whether the config was
    // actually written.

    // Per-invocation working directory. Claude CLI honors a project-local
    // `.claude/` dir at the invocation cwd: `.claude/mcp.json` configures
    // additional MCP servers, `.claude/skills/*.md` are loaded as skills.
    //
    // The workdir is keyed on the *dicode* session_id (which is already
    // shape-validated by SESSION_ID_RE above), NOT a fresh per-invocation
    // uuid. Reason: Claude CLI's conversation state is cwd-scoped — a
    // `--resume <claude_session_id>` invocation can only find sessions
    // that were created in the same working directory. If we used a
    // fresh uuid each turn, every `--resume` would fail with "No
    // conversation found".
    //
    // workdir_base resolves via the ${DATADIR} template variable at
    // spec-load time (see task.yaml), so we don't depend on
    // DICODE_DATA_DIR being set in the spawned env.
    //
    // Cleanup is handled out-of-band by buildin/temp-cleanup, which
    // sweeps ${DATADIR}/tmp/<task>/<uuid>/ leaves older than 1 hour
    // on its 10-minute cron. Active sessions keep their workdir mtime
    // fresh (we rewrite mcp.json + skills on every turn) so they're
    // never preempted; idle sessions are reaped after the TTL.
    if (!workdirBase) {
        return fail("workdir_base param is empty; expected the default ${DATADIR}/tmp/claude-cli to resolve at spec-load time");
    }
    const workdir = `${workdirBase}/${dicodeSessionId}`;
    const claudeDir = `${workdir}/.claude`;

    // Wrap setup in a try/catch so any unexpected error (NotCapable when
    // permissions.fs is mis-scoped, ENOSPC, an immutable mount, …) comes
    // back to the caller as a structured { ok: false, error } via fail()
    // rather than as an uncaught Deno promise rejection that the runtime
    // surfaces only in the run log.
    let mcpWired = false;
    let skillsWired = 0;
    const mcpKey = Deno.env.get("DICODE_MCP_API_KEY") ?? "";
    try {
        await Deno.mkdir(`${claudeDir}/skills`, { recursive: true });

        // MCP wiring: write .claude/mcp.json so Claude can call dicode tasks
        // as MCP tools. The API key is the same one the operator's local
        // Claude Code uses (stashed by `dicode mcp install`); empty = skip
        // and let Claude run without dicode-task tool access.
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
        // .claude/skills/. Names are validated against a strict regex to
        // defang any attempt at traversal via "../" or absolute paths.
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

    console.log(`ai-agent-claude-cli: invoking ${cliPath} (resume=${claudeSessionId ? claudeSessionId.slice(0, 8) + "…" : "no"}, model=${model || "default"}, dicode_session=${dicodeSessionId.slice(0, 8)}…, mcp=${mcpWired ? "on" : "off"}, skills=${skillsWired})`);

    const args = buildClaudeArgs({
        prompt,
        claudeSessionId,
        model,
        systemPrompt,
        // <cwd>/.claude/mcp.json is not auto-loaded; mount it explicitly and
        // strictly so only dicode's server reaches Claude.
        mcpConfigPath: mcpWired ? `${claudeDir}/mcp.json` : undefined,
    });

    const cmd = new Deno.Command(cliPath, {
        args,
        env: {
            CLAUDE_CODE_OAUTH_TOKEN: oauthToken,
            HOME: Deno.env.get("HOME") ?? "/root",
            PATH: Deno.env.get("PATH") ?? "/usr/local/bin:/usr/bin:/bin",
        },
        cwd: workdir,
        stdin: "null",
        stdout: "piped",
        stderr: "piped",
    });

    return await runClaudeAndDecode(cmd, oauthToken, mcpKey, kv, dicodeSessionId, claudeSessionId, model);
}

// runClaudeAndDecode is the post-setup half of main: spawn claude,
// parse the JSON response, persist the session-id mapping, and return
// the structured result. Split out so the surrounding try/finally in
// main() keeps cleanup logic together.
async function runClaudeAndDecode(
    cmd: Deno.Command,
    oauthToken: string,
    mcpKey: string,
    kv: KV,
    dicodeSessionId: string,
    claudeSessionId: string,
    fallbackModel: string,
): Promise<unknown> {
    let out: Deno.CommandOutput;
    try {
        out = await cmd.output();
    } catch (e) {
        return fail(`claude CLI invocation failed: ${e instanceof Error ? e.message : String(e)}`);
    }

    const stdout = new TextDecoder().decode(out.stdout).trim();
    const stderr = new TextDecoder().decode(out.stderr).trim();

    // redact strips secrets from any stream we surface to callers.
    // Uses replaceAll so multi-occurrence leaks (CLI repeats the
    // diagnostic) are fully scrubbed; String.replace would only catch
    // the first match. Both the OAuth token AND the dicode MCP API key
    // are sensitive — strip both, even though neither should ever leave
    // their respective stores.
    const redact = (s: string) => {
        let r = s;
        if (oauthToken) r = r.replaceAll(oauthToken, "<redacted>");
        if (mcpKey)     r = r.replaceAll(mcpKey,     "<redacted>");
        return r;
    };

    if (!out.success) {
        // Surface stderr for operator debugging; don't leak the OAuth
        // token even if the CLI ever logs it (it shouldn't, but defense
        // in depth).
        return fail(`claude exited ${out.code}: ${redact(stderr) || "(no stderr)"}`);
    }

    let parsed: ClaudeResponse;
    try {
        parsed = JSON.parse(stdout);
    } catch (_e) {
        // stdout is normally JSON; if Claude ever logs an OAuth-token-
        // bearing diagnostic to stdout instead, the redact() guard keeps
        // it out of the run log.
        return fail(`claude returned non-JSON output: ${redact(stdout.slice(0, 500))}`);
    }

    if (parsed.is_error) {
        return fail(parsed.result ?? "claude reported an error");
    }

    // Persist the dicode → claude session id mapping. KV writes are
    // fire-and-forget per the SDK shim (no ack); the next call with this
    // dicode_uuid will read it via kv.get above. If the CLI didn't return
    // a session_id (shouldn't happen in normal flow but defend against it),
    // we skip the write so a stale mapping isn't introduced.
    const newClaudeSessionId = parsed.session_id ?? "";
    if (newClaudeSessionId && newClaudeSessionId !== claudeSessionId) {
        try {
            await kv.set(KV_PREFIX + dicodeSessionId, newClaudeSessionId);
        } catch (e) {
            console.warn("ai-agent-claude-cli: kv.set failed (non-fatal): " + (e instanceof Error ? e.message : String(e)));
        }
    }

    console.log(`ai-agent-claude-cli: ok (model=${parsed.model ?? "?"}, cost=$${parsed.total_cost_usd ?? 0})`);

    return {
        ok:             true,
        reply:          parsed.result ?? "",
        session_id:     dicodeSessionId,
        model:          parsed.model ?? fallbackModel,
        total_cost_usd: parsed.total_cost_usd ?? 0,
        usage:          parsed.usage ?? null,
    };
}

// resolveCliPath returns an absolute path to the `claude` binary, or
// empty if none can be found. Order:
//   1. explicit cli_path param
//   2. CLAUDE_CLI_PATH env (set by the operator at daemon startup)
//   3. Deno.Command resolution against PATH
function resolveCliPath(cliPathParam: string): string {
    if (cliPathParam) return cliPathParam;
    const envPath = Deno.env.get("CLAUDE_CLI_PATH") ?? "";
    if (envPath) return envPath;
    // Lean on PATH resolution — Deno.Command will spawn `claude` from
    // PATH if we don't pass an absolute path. Returning the bare name
    // is enough; the actual lookup happens at exec time.
    return "claude";
}
