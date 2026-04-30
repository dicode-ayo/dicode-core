// deno-lint-ignore-file no-explicit-any
//
// buildin/ai-agent-claude-cli — wraps `claude -p` to call Claude with the
// operator's Pro/Max subscription instead of an Anthropic API key.
//
// Contract: returns { session_id, reply, model, total_cost_usd? } via
// dicode.output(). The session_id is generated server-side by Claude and
// can be passed back as session_id on subsequent invocations to continue
// the conversation.

const SESSION_ID_RE = /^[a-zA-Z0-9_-]{1,64}$/;

interface Params {
    prompt: string;
    session_id: string;
    model: string;
    system_prompt: string;
    cli_path: string;
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

export default async function main(opts: { params: Map<string, string>; dicode: any }) {
    const params: Params = {
        prompt:        opts.params.get("prompt")        ?? "",
        session_id:    opts.params.get("session_id")    ?? "",
        model:         opts.params.get("model")         ?? "",
        system_prompt: opts.params.get("system_prompt") ?? "",
        cli_path:      opts.params.get("cli_path")      ?? "",
    };

    if (!params.prompt) {
        return { ok: false, error: "prompt is required" };
    }
    // session_id is opaque to us, but reject anything obviously malformed
    // before it reaches the CLI's --resume flag (which would 4xx anyway).
    if (params.session_id && !SESSION_ID_RE.test(params.session_id)) {
        return { ok: false, error: `session_id ${JSON.stringify(params.session_id)} contains invalid characters; expected ^[a-zA-Z0-9_-]{1,64}$` };
    }

    // Refuse to invoke claude without an OAuth token. Without this guard
    // the CLI falls back to interactive OAuth or to ANTHROPIC_API_KEY (if
    // set) — both surprising in a daemon context. Be explicit: this task
    // is the subscription path; if the operator wants API-key auth they
    // should use buildin/ai-agent against the Anthropic OpenAI-compatible
    // endpoint instead.
    const oauthToken = Deno.env.get("CLAUDE_CODE_OAUTH_TOKEN") ?? "";
    if (!oauthToken) {
        return {
            ok: false,
            error: "CLAUDE_CODE_OAUTH_TOKEN is not set. Mint one via `claude setup-token` and store it as a dicode secret named CLAUDE_CODE_OAUTH_TOKEN. See tasks/buildin/ai-agent-claude-cli/README.md.",
        };
    }

    const cliPath = resolveCliPath(params.cli_path);
    if (!cliPath) {
        return {
            ok: false,
            error: "claude binary not found. Set the cli_path param, or install via one of the paths in tasks/buildin/ai-agent-claude-cli/README.md.",
        };
    }

    // Build args. The -p / --print mode runs one-shot and emits JSON on
    // stdout, no interactive shell. --output-format json gives us the
    // structured response (vs. plain text).
    const args = ["-p", params.prompt, "--output-format", "json"];
    if (params.session_id) {
        args.push("--resume", params.session_id);
    }
    if (params.model) {
        args.push("--model", params.model);
    }
    if (params.system_prompt) {
        args.push("--append-system-prompt", params.system_prompt);
    }

    const cmd = new Deno.Command(cliPath, {
        args,
        env: {
            CLAUDE_CODE_OAUTH_TOKEN: oauthToken,
            HOME: Deno.env.get("HOME") ?? "/root",
            PATH: Deno.env.get("PATH") ?? "/usr/local/bin:/usr/bin:/bin",
        },
        stdin: "null",
        stdout: "piped",
        stderr: "piped",
    });

    let out: Deno.CommandOutput;
    try {
        out = await cmd.output();
    } catch (e) {
        return { ok: false, error: `claude CLI invocation failed: ${e instanceof Error ? e.message : String(e)}` };
    }

    const stdout = new TextDecoder().decode(out.stdout).trim();
    const stderr = new TextDecoder().decode(out.stderr).trim();

    if (!out.success) {
        // Surface stderr for operator debugging; don't leak the OAuth
        // token even if the CLI ever logs it (it shouldn't, but defense
        // in depth).
        const safeErr = stderr.replace(oauthToken, "<redacted>");
        return { ok: false, error: `claude exited ${out.code}: ${safeErr || "(no stderr)"}` };
    }

    let parsed: ClaudeResponse;
    try {
        parsed = JSON.parse(stdout);
    } catch (_e) {
        return { ok: false, error: `claude returned non-JSON output: ${stdout.slice(0, 500)}` };
    }

    if (parsed.is_error) {
        return { ok: false, error: parsed.result ?? "claude reported an error", details: parsed };
    }

    return {
        ok:             true,
        reply:          parsed.result ?? "",
        session_id:     parsed.session_id ?? "",
        model:          parsed.model ?? params.model,
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
