// Tests for buildin/ai-agent-claude-cli — verify the one-shot single-turn
// path, the JSON parsing path, and the suspend/resume chat loop. The actual
// `claude` CLI is stubbed via PATH manipulation; tests don't need a real
// binary.

import { assertEquals } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main, { buildClaudeArgs, decideEntryMode, isChatEnd, steps } from "./task.ts";
import { isValidSessionId } from "../ai-agent-core/chat.ts";

const fakeDicode = {} as any;

// A dicode stub whose suspend() records the request and throws a sentinel —
// dicode.suspend never returns in production (the process exits), so tests must
// stop execution the same way and inspect the recorded suspend calls.
class SuspendSignal extends Error {}
function makeSuspendDicode() {
    const calls: any[] = [];
    return {
        calls,
        dicode: {
            suspend: (req: any) => { calls.push(req); throw new SuspendSignal(); },
        } as any,
    };
}

// makeParams returns an SDK-shaped Params object whose .get() is async,
// matching pkg/runtime/deno/sdk/shim.ts. The task code awaits these calls;
// passing a plain Map<string,string> would surface as Promise objects in
// every await target and break validation in surprising ways.
//
// workdir_base is template-resolved at spec-load (${DATADIR}/...) in
// production but unit tests don't go through that path. Inject a
// sensible default rooted at DICODE_DATA_DIR (set by withStubClaude
// for every test that proceeds past validation) so individual tests
// don't have to thread the param through every call site.
function makeParams(entries: Array<[string, string]>) {
    const m = new Map(entries);
    if (!m.has("workdir_base")) {
        const dataDir = Deno.env.get("DICODE_DATA_DIR") ?? "";
        if (dataDir) m.set("workdir_base", `${dataDir}/tmp/claude-cli`);
    }
    return {
        get: (k: string) => Promise.resolve(m.get(k) ?? null),
        all: () => Promise.resolve(Object.fromEntries(m)),
    };
}

async function withStubClaude<T>(
    stubBody: string,
    fn: () => Promise<T>,
): Promise<T> {
    const tmp = await Deno.makeTempDir();
    const stub = `${tmp}/claude`;
    await Deno.writeTextFile(stub, `#!/bin/sh
${stubBody}
`);
    await Deno.chmod(stub, 0o755);
    const origPath    = Deno.env.get("PATH") ?? "";
    const origDataDir = Deno.env.get("DICODE_DATA_DIR");
    Deno.env.set("PATH", `${tmp}:${origPath}`);
    Deno.env.set("CLAUDE_CLI_PATH", stub);
    // DICODE_DATA_DIR drives the per-invocation .claude/ workdir in
    // task.ts. Pin it to the same per-test tempdir so workdirs are
    // created and cleaned up under our control.
    Deno.env.set("DICODE_DATA_DIR", tmp);
    try {
        return await fn();
    } finally {
        Deno.env.set("PATH", origPath);
        Deno.env.delete("CLAUDE_CLI_PATH");
        if (origDataDir === undefined) {
            Deno.env.delete("DICODE_DATA_DIR");
        } else {
            Deno.env.set("DICODE_DATA_DIR", origDataDir);
        }
    }
}

Deno.test("no prompt on a fresh run opens the chat loop (suspends to turn)", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const { calls, dicode } = makeSuspendDicode();
    let signalled = false;
    try {
        await main({ params: makeParams([]), dicode });
    } catch (e) {
        if (e instanceof SuspendSignal) signalled = true;
        else throw e;
    }
    assertEquals(signalled, true);
    assertEquals(calls.length, 1);
    assertEquals(calls[0].to, "turn");
    assertEquals(calls[0].state.claudeSessionId, "");
    // `message` is intentionally NOT required so a blank line ends the chat.
    assertEquals(calls[0].schema.required, undefined);
    assertEquals(typeof calls[0].schema.properties.message, "object");
    // chatId keys the workdir across turns; must be seeded on the first suspend.
    if (typeof calls[0].state.chatId !== "string" || !calls[0].state.chatId) {
        throw new Error(`expected a seeded chatId, got ${calls[0].state.chatId}`);
    }
});

Deno.test("no token: falls back to logged-in credentials (does not hard-fail)", async () => {
    Deno.env.delete("CLAUDE_CODE_OAUTH_TOKEN");
    const result: any = await withStubClaude(
        `cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"s"}
JSON`,
        () => main({ params: makeParams([["prompt", "hi"]]), dicode: fakeDicode }),
    );
    assertEquals(result.ok, true);
});

Deno.test("no token: does not inject an empty CLAUDE_CODE_OAUTH_TOKEN into claude env", async () => {
    // Setting the var to "" would override the $HOME/.claude credential-file
    // fallback with an invalid empty token — it must be absent, not empty.
    Deno.env.delete("CLAUDE_CODE_OAUTH_TOKEN");
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/token-seen`;
    await withStubClaude(
        `printf '[%s]' "\${CLAUDE_CODE_OAUTH_TOKEN-UNSET}" > ${sentinel}
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"s"}
JSON`,
        () => main({ params: makeParams([["prompt", "hi"]]), dicode: fakeDicode }),
    );
    assertEquals((await Deno.readTextFile(sentinel)).trim(), "[UNSET]");
});

Deno.test("happy path returns reply + session_id", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const result: any = await withStubClaude(
        `cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"hello world","session_id":"sess-abc123","model":"claude-sonnet-4","total_cost_usd":0.001}
JSON`,
        () =>
            main({
                params: makeParams([["prompt", "say hello"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, true);
    assertEquals(result.reply, "hello world");
    assertEquals(result.model, "claude-sonnet-4");
    // One-shot is stateless: the returned session_id is a fresh UUID keying the
    // per-invocation workdir, not the Claude CLI's own id.
    if (!isValidSessionId(result.session_id)) {
        throw new Error(`expected uuid-shaped session_id, got ${result.session_id}`);
    }
});

Deno.test("surfaces is_error: true responses", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const result: any = await withStubClaude(
        `cat <<'JSON'
{"type":"result","is_error":true,"result":"rate limited"}
JSON`,
        () =>
            main({
                params: makeParams([["prompt", "anything"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, false);
    if (!String(result.error ?? "").includes("rate limited")) {
        throw new Error(`expected rate-limited error, got ${result.error}`);
    }
});

Deno.test("surfaces non-zero exit code with stderr", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const result: any = await withStubClaude(
        `echo "auth failed: bad token" >&2
exit 2`,
        () =>
            main({
                params: makeParams([["prompt", "anything"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, false);
    if (!String(result.error ?? "").includes("auth failed")) {
        throw new Error(`expected stderr propagation, got ${result.error}`);
    }
});

Deno.test("redacts OAuth token if it leaks into stderr", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "supersecret-token-xyz");
    const result: any = await withStubClaude(
        `echo "diagnostic: token=supersecret-token-xyz failed" >&2
exit 1`,
        () =>
            main({
                params: makeParams([["prompt", "anything"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, false);
    if (String(result.error ?? "").includes("supersecret-token-xyz")) {
        throw new Error(`OAuth token leaked into error: ${result.error}`);
    }
    if (!String(result.error ?? "").includes("<redacted>")) {
        throw new Error(`expected <redacted> placeholder, got ${result.error}`);
    }
});

Deno.test("redacts every occurrence of the token, not just the first", async () => {
    // Regression: String.replace only swaps the first match. If Claude
    // ever logs the OAuth token twice (or more), the trailing copies
    // would have leaked through. Use of replaceAll defends against that.
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "supersecret-token-xyz");
    const result: any = await withStubClaude(
        `echo "first: supersecret-token-xyz; second: supersecret-token-xyz" >&2
exit 1`,
        () =>
            main({
                params: makeParams([["prompt", "anything"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, false);
    const err = String(result.error ?? "");
    if (err.includes("supersecret-token-xyz")) {
        throw new Error(`OAuth token leaked (one occurrence missed): ${err}`);
    }
    // Both occurrences should be replaced; expect "<redacted>" twice.
    const matches = err.match(/<redacted>/g) ?? [];
    if (matches.length < 2) {
        throw new Error(`expected 2 <redacted> placeholders, got ${matches.length} in: ${err}`);
    }
});

Deno.test("redacts token in JSON-parse-failure path too", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "supersecret-token-xyz");
    const result: any = await withStubClaude(
        `echo "non-JSON output mentioning supersecret-token-xyz somehow"`,
        () =>
            main({
                params: makeParams([["prompt", "anything"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, false);
    if (String(result.error ?? "").includes("supersecret-token-xyz")) {
        throw new Error(`OAuth token leaked via stdout/JSON-parse path: ${result.error}`);
    }
});

Deno.test("writes .claude/mcp.json when DICODE_MCP_API_KEY is set", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    Deno.env.set("DICODE_MCP_API_KEY", "dck_test_mcp_key");
    // Stub records cwd into a sentinel file so we can read it back AFTER
    // the task's finally-block runs (which removes the workdir). The
    // sentinel writes to /tmp/<pwd-basename> which survives cleanup.
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/cwd-recorded`;
    const result: any = await withStubClaude(
        `cat "$PWD/.claude/mcp.json" > ${sentinel} 2>/dev/null || true
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"s"}
JSON`,
        () =>
            main({
                params: makeParams([["prompt", "hi"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, true);
    const recorded = await Deno.readTextFile(sentinel);
    const cfg = JSON.parse(recorded);
    if (!cfg.mcpServers?.dicode?.headers?.Authorization?.includes("dck_test_mcp_key")) {
        throw new Error(`expected dicode MCP key in .claude/mcp.json, got ${recorded}`);
    }
    Deno.env.delete("DICODE_MCP_API_KEY");
});

Deno.test("skips MCP wiring when DICODE_MCP_API_KEY is empty", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    Deno.env.delete("DICODE_MCP_API_KEY");
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/mcp-exists`;
    const result: any = await withStubClaude(
        `[ -f "$PWD/.claude/mcp.json" ] && echo "yes" > ${sentinel} || echo "no" > ${sentinel}
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"s"}
JSON`,
        () =>
            main({
                params: makeParams([["prompt", "hi"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, true);
    const recorded = (await Deno.readTextFile(sentinel)).trim();
    assertEquals(recorded, "no");
});

Deno.test("rejects path-traversal in skills param", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const skillsDir = await Deno.makeTempDir();
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/skills-listed`;
    const result: any = await withStubClaude(
        `ls "$PWD/.claude/skills" > ${sentinel} 2>/dev/null || echo "(empty)" > ${sentinel}
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"s"}
JSON`,
        () =>
            main({
                params: makeParams([
                    ["prompt", "hi"],
                    ["skills", "../../etc/passwd,legit-skill"],
                    ["skills_dir", skillsDir],
                ]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, true);
    const listed = (await Deno.readTextFile(sentinel)).trim();
    if (listed.includes("passwd")) {
        throw new Error(`path-traversal slipped through; .claude/skills lists: ${listed}`);
    }
});

Deno.test("buildClaudeArgs: base args are always present", () => {
    assertEquals(
        buildClaudeArgs({ prompt: "hi" }).join(" "),
        "-p hi --output-format json --disallowedTools Bash,Read,Write,Edit,NotebookEdit,WebFetch,WebSearch,Glob,Grep,Task,KillShell",
    );
});

Deno.test("buildClaudeArgs: resume/model/system_prompt appended when set", () => {
    const a = buildClaudeArgs({
        prompt: "hi", claudeSessionId: "s1", model: "sonnet", systemPrompt: "sp",
    });
    assertEquals(a[a.indexOf("--resume") + 1], "s1");
    assertEquals(a[a.indexOf("--model") + 1], "sonnet");
    assertEquals(a[a.indexOf("--append-system-prompt") + 1], "sp");
});

Deno.test("buildClaudeArgs: no MCP flags when no config path (mount skipped)", () => {
    const a = buildClaudeArgs({ prompt: "hi" });
    assertEquals(a.includes("--mcp-config"), false);
    assertEquals(a.includes("--strict-mcp-config"), false);
});

Deno.test("buildClaudeArgs: mounts MCP strictly + explicitly with a config path", () => {
    const a = buildClaudeArgs({ prompt: "hi", mcpConfigPath: "/w/.claude/mcp.json" });
    assertEquals(a.includes("--strict-mcp-config"), true);
    const i = a.indexOf("--mcp-config");
    assertEquals(a[i + 1], "/w/.claude/mcp.json");
});

Deno.test("buildClaudeArgs: allowlists the dicode MCP tools when MCP is wired", () => {
    // Without this, `claude -p` refuses un-allowlisted tools (can't prompt),
    // so it sees the dicode tools but never calls them. Scoped to `mcp__dicode`
    // so the agent gets dicode's surface, not raw Bash/Write host access.
    const a = buildClaudeArgs({ prompt: "hi", mcpConfigPath: "/w/.claude/mcp.json" });
    const i = a.indexOf("--allowedTools");
    assertEquals(i >= 0, true);
    assertEquals(a[i + 1], "mcp__dicode");
    // No allowlist flag when MCP isn't mounted.
    assertEquals(buildClaudeArgs({ prompt: "hi" }).includes("--allowedTools"), false);
});

Deno.test("buildClaudeArgs: denies the dangerous built-in tools unconditionally (#560 fail-closed)", () => {
    // The bug this guards: when mcpConfigPath is unset (MCP disabled, or MCP
    // wiring failed), buildClaudeArgs used to push no --allowedTools AND no
    // --disallowedTools, so Claude silently fell back to its full default
    // toolset — Bash/Read/Write/Edit/etc — with real host fs+exec access as
    // the daemon's OS user. --disallowedTools must be present, and must deny
    // every dangerous built-in tool, regardless of MCP wiring state.
    const dangerous = [
        "Bash", "Read", "Write", "Edit", "NotebookEdit", "WebFetch", "WebSearch",
        "Glob", "Grep", "Task", "KillShell",
    ];

    const withoutMcp = buildClaudeArgs({ prompt: "hi" });
    const iNoMcp = withoutMcp.indexOf("--disallowedTools");
    assertEquals(iNoMcp >= 0, true);
    for (const tool of dangerous) {
        assertEquals(withoutMcp[iNoMcp + 1].includes(tool), true);
    }

    const withMcp = buildClaudeArgs({ prompt: "hi", mcpConfigPath: "/w/.claude/mcp.json" });
    const iMcp = withMcp.indexOf("--disallowedTools");
    assertEquals(iMcp >= 0, true);
    for (const tool of dangerous) {
        assertEquals(withMcp[iMcp + 1].includes(tool), true);
    }
    // The MCP-wired path keeps its existing allowlist alongside the denylist.
    assertEquals(withMcp.includes("--allowedTools"), true);
});

Deno.test("passes --strict-mcp-config --mcp-config to claude when MCP is wired", async () => {
    // The bug this guards: previously the config was written to
    // <cwd>/.claude/mcp.json with no flag, which the CLI never auto-loads, so
    // Claude ran with zero dicode tools. Assert the flags reach the binary.
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    Deno.env.set("DICODE_MCP_API_KEY", "dck_test_mcp_key");
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/args-recorded`;
    const result: any = await withStubClaude(
        `printf '%s\\n' "$@" > ${sentinel}
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"s"}
JSON`,
        () => main({ params: makeParams([["prompt", "hi"]]), dicode: fakeDicode }),
    );
    assertEquals(result.ok, true);
    const recorded = await Deno.readTextFile(sentinel);
    if (!recorded.includes("--strict-mcp-config")) {
        throw new Error(`expected --strict-mcp-config in claude args, got:\n${recorded}`);
    }
    const lines = recorded.split("\n");
    const i = lines.indexOf("--mcp-config");
    if (i < 0 || !lines[i + 1]?.endsWith("/.claude/mcp.json")) {
        throw new Error(`expected --mcp-config <…/.claude/mcp.json>, got:\n${recorded}`);
    }
    Deno.env.delete("DICODE_MCP_API_KEY");
});

// --- chat loop -----------------------------------------------------------

Deno.test("decideEntryMode: non-empty prompt → one-shot, empty → chat-start", () => {
    assertEquals(decideEntryMode("hi"), "one-shot");
    assertEquals(decideEntryMode(""), "chat-start");
    assertEquals(decideEntryMode("   "), "chat-start");
});

Deno.test("isChatEnd: blank/whitespace ends, content continues", () => {
    assertEquals(isChatEnd(""), true);
    assertEquals(isChatEnd("   "), true);
    assertEquals(isChatEnd("\n\t"), true);
    assertEquals(isChatEnd("hello"), false);
});

// UUID-shaped fixtures: chatId/claudeSessionId now go through isValidSessionId
// before use, so carried-state fixtures in these tests must be UUID-shaped to
// exercise the "valid, passed through" path. The off-shape/rejection path is
// covered separately below.
const VALID_CHAT_ID = "11111111-1111-1111-1111-111111111111";
const VALID_PRIOR_SESSION_ID = "33333333-3333-3333-3333-333333333333";

Deno.test("steps.turn: a blank message ends the chat (returns, no Claude call)", async () => {
    const { calls, dicode } = makeSuspendDicode();
    const result: any = await steps.turn({
        params: makeParams([]),
        input: { message: "   " },
        state: { claudeSessionId: VALID_PRIOR_SESSION_ID, chatId: "c1" },
        dicode,
        output: {} as any,
        mcp: {} as any,
    } as any);
    assertEquals(result.ok, true);
    assertEquals(result.reply, "(chat ended)");
    assertEquals(result.session_id, VALID_PRIOR_SESSION_ID);
    assertEquals(calls.length, 0); // never suspended onward
});

Deno.test("steps.turn: a blank message ending the chat rejects an invalid carried claudeSessionId instead of echoing it", async () => {
    // The onEnd path (chatTurn's isChatEnd short-circuit) never runs runTurn,
    // so it has its own resolveSessionId call — this guards that it's actually
    // wired up, not just the non-blank-message path covered above.
    const { calls, dicode } = makeSuspendDicode();
    const result: any = await steps.turn({
        params: makeParams([]),
        input: { message: "" },
        state: { claudeSessionId: "--dangerously-skip-permissions", chatId: VALID_CHAT_ID },
        dicode,
        output: {} as any,
        mcp: {} as any,
    } as any);
    assertEquals(result.ok, true);
    assertEquals(result.reply, "(chat ended)");
    assertEquals(result.session_id, "");
    assertEquals(calls.length, 0);
});

Deno.test("steps.turn: a message runs one turn and suspends back with the reply", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const { calls, dicode } = makeSuspendDicode();
    let signalled = false;
    await withStubClaude(
        `cat <<'JSON'
{"type":"result","is_error":false,"result":"pong","session_id":"sess-new","model":"claude-sonnet-4"}
JSON`,
        async () => {
            try {
                await steps.turn({
                    params: makeParams([]),
                    input: { message: "ping" },
                    state: { claudeSessionId: "", chatId: VALID_CHAT_ID },
                    dicode,
                    output: {} as any,
                    mcp: {} as any,
                } as any);
            } catch (e) {
                if (e instanceof SuspendSignal) signalled = true;
                else throw e;
            }
        },
    );
    assertEquals(signalled, true);
    assertEquals(calls.length, 1);
    assertEquals(calls[0].to, "turn");
    // Claude's new session id is carried forward; chatId (the workdir key) is stable.
    assertEquals(calls[0].state.claudeSessionId, "sess-new");
    assertEquals(calls[0].state.chatId, VALID_CHAT_ID);
    // The reply becomes the next prompt's banner — on the schema and the field.
    assertEquals(calls[0].schema.description, "pong");
    assertEquals(calls[0].schema.properties.message.description, "pong");
});

Deno.test("steps.turn: resumes Claude's prior session via --resume, and chatId stays stable", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const { calls, dicode } = makeSuspendDicode();
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/args-recorded`;
    await withStubClaude(
        `printf '%s\\n' "$@" > ${sentinel}
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"sess-2"}
JSON`,
        async () => {
            try {
                await steps.turn({
                    params: makeParams([]),
                    input: { message: "again" },
                    state: { claudeSessionId: VALID_PRIOR_SESSION_ID, chatId: VALID_CHAT_ID },
                    dicode,
                    output: {} as any,
                    mcp: {} as any,
                } as any);
            } catch (e) {
                if (!(e instanceof SuspendSignal)) throw e;
            }
        },
    );
    const lines = (await Deno.readTextFile(sentinel)).split("\n");
    const i = lines.indexOf("--resume");
    if (i < 0 || lines[i + 1] !== VALID_PRIOR_SESSION_ID) {
        throw new Error(`expected --resume ${VALID_PRIOR_SESSION_ID}, got:\n${lines.join(" ")}`);
    }
    // A valid carried chatId must pass through unchanged (not silently
    // replaced), same as the workdir it keys.
    assertEquals(calls[0].state.chatId, VALID_CHAT_ID);
});

Deno.test("steps.turn: rejects a path-traversal chatId in resume state; falls back to a fresh UUID workdir", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const { dicode } = makeSuspendDicode();
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/cwd-recorded`;
    await withStubClaude(
        `pwd > ${sentinel}
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"sess-new"}
JSON`,
        async () => {
            try {
                await steps.turn({
                    params: makeParams([]),
                    input: { message: "hi" },
                    state: { claudeSessionId: "", chatId: "../../../../tmp/dicode-chatid-escape" },
                    dicode,
                    output: {} as any,
                    mcp: {} as any,
                } as any);
            } catch (e) {
                if (!(e instanceof SuspendSignal)) throw e;
            }
        },
    );
    const recordedCwd = (await Deno.readTextFile(sentinel)).trim();
    if (recordedCwd.includes("dicode-chatid-escape")) {
        throw new Error(`chatId path traversal reached the subprocess cwd: ${recordedCwd}`);
    }
    const basename = recordedCwd.split("/").pop() ?? "";
    if (!isValidSessionId(basename)) {
        throw new Error(`expected a fresh UUID-shaped workdir, got cwd: ${recordedCwd}`);
    }
});

Deno.test("steps.turn: rejects a flag-shaped claudeSessionId in resume state; omits --resume entirely", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const { dicode } = makeSuspendDicode();
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/args-recorded`;
    await withStubClaude(
        `printf '%s\\n' "$@" > ${sentinel}
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"sess-new"}
JSON`,
        async () => {
            try {
                await steps.turn({
                    params: makeParams([]),
                    input: { message: "hi" },
                    // A crafted resume submission trying to smuggle an extra CLI flag
                    // in as the "session id".
                    state: { claudeSessionId: "--dangerously-skip-permissions", chatId: VALID_CHAT_ID },
                    dicode,
                    output: {} as any,
                    mcp: {} as any,
                } as any);
            } catch (e) {
                if (!(e instanceof SuspendSignal)) throw e;
            }
        },
    );
    const args = (await Deno.readTextFile(sentinel)).split("\n");
    if (args.includes("--resume")) {
        throw new Error(`expected --resume to be omitted for an invalid carried claudeSessionId, got:\n${args.join(" ")}`);
    }
    if (args.includes("--dangerously-skip-permissions")) {
        throw new Error(`malicious claudeSessionId leaked into claude args:\n${args.join(" ")}`);
    }
});

Deno.test("steps.turn: an invalid chatId also drops a valid carried claudeSessionId (cwd-scoped --resume would otherwise fail)", async () => {
    // Regression for a bug caught in review: Claude CLI sessions are
    // cwd-scoped, so pairing a fresh workdir (minted because chatId was
    // off-shape) with the OLD claudeSessionId would make --resume fail
    // ("No conversation found") instead of gracefully starting fresh.
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const { dicode } = makeSuspendDicode();
    const sentinelDir = await Deno.makeTempDir();
    const sentinel = `${sentinelDir}/args-recorded`;
    await withStubClaude(
        `printf '%s\\n' "$@" > ${sentinel}
cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"sess-new"}
JSON`,
        async () => {
            try {
                await steps.turn({
                    params: makeParams([]),
                    input: { message: "hi" },
                    state: { claudeSessionId: VALID_PRIOR_SESSION_ID, chatId: "not-a-uuid" },
                    dicode,
                    output: {} as any,
                    mcp: {} as any,
                } as any);
            } catch (e) {
                if (!(e instanceof SuspendSignal)) throw e;
            }
        },
    );
    const args = (await Deno.readTextFile(sentinel)).split("\n");
    if (args.includes("--resume")) {
        throw new Error(`expected --resume to be omitted when chatId had to be replaced, got:\n${args.join(" ")}`);
    }
});
