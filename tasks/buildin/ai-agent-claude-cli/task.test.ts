// Tests for buildin/ai-agent-claude-cli — verify input validation and
// the JSON parsing path. The actual `claude` CLI is stubbed via PATH
// manipulation; tests don't need a real binary.

import { assertEquals } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main from "./task.ts";

const fakeDicode = {} as any;

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
    const origPath = Deno.env.get("PATH") ?? "";
    Deno.env.set("PATH", `${tmp}:${origPath}`);
    Deno.env.set("CLAUDE_CLI_PATH", stub);
    try {
        return await fn();
    } finally {
        Deno.env.set("PATH", origPath);
        Deno.env.delete("CLAUDE_CLI_PATH");
    }
}

Deno.test("rejects empty prompt", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const r = await main({ params: new Map(), dicode: fakeDicode });
    assertEquals(r.ok, false);
});

Deno.test("rejects missing OAuth token", async () => {
    Deno.env.delete("CLAUDE_CODE_OAUTH_TOKEN");
    const r = await main({
        params: new Map([["prompt", "hi"]]),
        dicode: fakeDicode,
    });
    assertEquals(r.ok, false);
    if (!String(r.error ?? "").includes("CLAUDE_CODE_OAUTH_TOKEN")) {
        throw new Error(`expected OAuth-token error, got ${r.error}`);
    }
});

Deno.test("rejects malformed session_id", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const r = await main({
        params: new Map([
            ["prompt", "hi"],
            ["session_id", "../etc/passwd"],
        ]),
        dicode: fakeDicode,
    });
    assertEquals(r.ok, false);
    if (!String(r.error ?? "").includes("invalid characters")) {
        throw new Error(`expected invalid-characters error, got ${r.error}`);
    }
});

Deno.test("happy path returns reply + session_id", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const result = await withStubClaude(
        `cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"hello world","session_id":"sess-abc123","model":"claude-sonnet-4","total_cost_usd":0.001}
JSON`,
        () =>
            main({
                params: new Map([["prompt", "say hello"]]),
                dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, true);
    assertEquals(result.reply, "hello world");
    assertEquals(result.session_id, "sess-abc123");
    assertEquals(result.model, "claude-sonnet-4");
});

Deno.test("surfaces is_error: true responses", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const result = await withStubClaude(
        `cat <<'JSON'
{"type":"result","is_error":true,"result":"rate limited"}
JSON`,
        () =>
            main({
                params: new Map([["prompt", "anything"]]),
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
    const result = await withStubClaude(
        `echo "auth failed: bad token" >&2
exit 2`,
        () =>
            main({
                params: new Map([["prompt", "anything"]]),
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
    const result = await withStubClaude(
        `echo "diagnostic: token=supersecret-token-xyz failed" >&2
exit 1`,
        () =>
            main({
                params: new Map([["prompt", "anything"]]),
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
