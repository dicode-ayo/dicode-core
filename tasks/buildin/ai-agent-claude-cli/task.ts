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

const SESSION_ID_RE = /^[a-zA-Z0-9_-]{1,64}$/;
// kvKey prefix; keep separate from buildin/ai-agent's "chat:<id>" namespace.
const KV_PREFIX = "claude:";

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

export default async function main(opts: { params: Map<string, string>; kv: any; dicode: any }) {
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
    // session_id shape — dicode side. Reject anything not UUID-shaped so a
    // hostile caller can't forge a key that hits arbitrary KV entries.
    if (params.session_id && !SESSION_ID_RE.test(params.session_id)) {
        return { ok: false, error: `session_id ${JSON.stringify(params.session_id)} contains invalid characters; expected ^[a-zA-Z0-9_-]{1,64}$` };
    }

    // dicode-side session id. Generate one if the caller didn't pass one
    // (matches buildin/ai-agent's UX: first turn returns a fresh uuid).
    const dicodeSessionId = params.session_id || crypto.randomUUID();

    // Look up the Claude-side session id for this dicode session, if any.
    // First call: kv miss → no --resume → CLI mints a new Claude session.
    let claudeSessionId = "";
    try {
        const stored = await opts.kv.get(KV_PREFIX + dicodeSessionId);
        if (typeof stored === "string") claudeSessionId = stored;
    } catch (_) {
        // KV miss is fine; a transient KV error is not — but since this is
        // a one-shot read and a missing entry is indistinguishable from a
        // string-typed null, we treat any error as "no prior session".
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
    if (claudeSessionId) {
        args.push("--resume", claudeSessionId);
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

    // Persist the dicode → claude session id mapping. KV writes are
    // fire-and-forget per the SDK shim (no ack); the next call with this
    // dicode_uuid will read it via kv.get above. If the CLI didn't return
    // a session_id (shouldn't happen in normal flow but defend against it),
    // we skip the write so a stale mapping isn't introduced.
    const newClaudeSessionId = parsed.session_id ?? "";
    if (newClaudeSessionId && newClaudeSessionId !== claudeSessionId) {
        try {
            opts.kv.set(KV_PREFIX + dicodeSessionId, newClaudeSessionId);
        } catch (_) {
            // KV write failure is non-fatal — the user can retry; worst
            // case the next call starts a fresh Claude session.
        }
    }

    return {
        ok:             true,
        reply:          parsed.result ?? "",
        session_id:     dicodeSessionId,                  // dicode-side id, opaque to caller
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
