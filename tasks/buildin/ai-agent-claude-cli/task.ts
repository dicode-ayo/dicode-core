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

export default async function main({ params, kv }: SDK) {
    const prompt        = (await params.get("prompt"))        ?? "";
    const sessionIdIn   = (await params.get("session_id"))    ?? "";
    const model         = (await params.get("model"))         ?? "";
    const systemPrompt  = (await params.get("system_prompt")) ?? "";
    const cliPathParam  = (await params.get("cli_path"))      ?? "";

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
    let claudeSessionId = "";
    try {
        const stored = await kv.get(KV_PREFIX + dicodeSessionId);
        if (typeof stored === "string") claudeSessionId = stored;
    } catch (e) {
        // KV miss is normal; transient KV errors are non-fatal — worst
        // case the next turn starts a fresh Claude session.
        console.warn("ai-agent-claude-cli: kv.get failed: " + (e instanceof Error ? e.message : String(e)));
    }

    // Build args. The -p / --print mode runs one-shot and emits JSON on
    // stdout, no interactive shell. --output-format json gives us the
    // structured response (vs. plain text).
    const args = ["-p", prompt, "--output-format", "json"];
    if (claudeSessionId) {
        args.push("--resume", claudeSessionId);
    }
    if (model) {
        args.push("--model", model);
    }
    if (systemPrompt) {
        args.push("--append-system-prompt", systemPrompt);
    }

    console.log(`ai-agent-claude-cli: invoking ${cliPath} (resume=${claudeSessionId ? claudeSessionId.slice(0, 8) + "…" : "no"}, model=${model || "default"}, dicode_session=${dicodeSessionId.slice(0, 8)}…)`);

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
        return fail(`claude CLI invocation failed: ${e instanceof Error ? e.message : String(e)}`);
    }

    const stdout = new TextDecoder().decode(out.stdout).trim();
    const stderr = new TextDecoder().decode(out.stderr).trim();

    if (!out.success) {
        // Surface stderr for operator debugging; don't leak the OAuth
        // token even if the CLI ever logs it (it shouldn't, but defense
        // in depth).
        const safeErr = stderr.replace(oauthToken, "<redacted>");
        return fail(`claude exited ${out.code}: ${safeErr || "(no stderr)"}`);
    }

    let parsed: ClaudeResponse;
    try {
        parsed = JSON.parse(stdout);
    } catch (_e) {
        return fail(`claude returned non-JSON output: ${stdout.slice(0, 500)}`);
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
        model:          parsed.model ?? model,
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
