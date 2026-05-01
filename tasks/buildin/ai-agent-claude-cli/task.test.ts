// Tests for buildin/ai-agent-claude-cli — verify input validation,
// the JSON parsing path, and the dicode↔claude session_id mapping
// stored via kv. The actual `claude` CLI is stubbed via PATH
// manipulation; tests don't need a real binary.

import { assertEquals } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main from "./task.ts";

const fakeDicode = {} as any;

// makeParams returns an SDK-shaped Params object whose .get() is async,
// matching pkg/runtime/deno/sdk/shim.ts. The task code awaits these calls;
// passing a plain Map<string,string> would surface as Promise objects in
// every await target and break validation in surprising ways.
function makeParams(entries: Array<[string, string]>) {
    const m = new Map(entries);
    return {
        get: (k: string) => Promise.resolve(m.get(k) ?? null),
        all: () => Promise.resolve(Object.fromEntries(m)),
    };
}

function makeKv() {
    const store = new Map<string, unknown>();
    return {
        store,
        get: (k: string) => Promise.resolve(store.get(k) ?? null),
        set: (k: string, v: unknown) => { store.set(k, v); return Promise.resolve(); },
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
    const r = await main({ params: makeParams([]), kv: makeKv(), dicode: fakeDicode });
    assertEquals(r.ok, false);
});

Deno.test("rejects missing OAuth token", async () => {
    Deno.env.delete("CLAUDE_CODE_OAUTH_TOKEN");
    const r = await main({
        params: makeParams([["prompt", "hi"]]),
        kv: makeKv(), dicode: fakeDicode,
    });
    assertEquals(r.ok, false);
    if (!String(r.error ?? "").includes("CLAUDE_CODE_OAUTH_TOKEN")) {
        throw new Error(`expected OAuth-token error, got ${r.error}`);
    }
});

Deno.test("rejects malformed session_id", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const r = await main({
        params: makeParams([
            ["prompt", "hi"],
            ["session_id", "../etc/passwd"],
        ]),
        kv: makeKv(), dicode: fakeDicode,
    });
    assertEquals(r.ok, false);
    if (!String(r.error ?? "").includes("invalid characters")) {
        throw new Error(`expected invalid-characters error, got ${r.error}`);
    }
});

Deno.test("happy path returns reply + session_id", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const kv = makeKv();
    const result = await withStubClaude(
        `cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"hello world","session_id":"sess-abc123","model":"claude-sonnet-4","total_cost_usd":0.001}
JSON`,
        () =>
            main({
                params: makeParams([["prompt", "say hello"]]),
                kv, dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, true);
    assertEquals(result.reply, "hello world");
    assertEquals(result.model, "claude-sonnet-4");
    // Dicode-side session_id is a fresh UUID — the Claude CLI's id stays
    // server-side via the kv mapping below.
    if (!/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/i.test(result.session_id)) {
        throw new Error(`expected uuid-shaped session_id, got ${result.session_id}`);
    }
    assertEquals(kv.store.get("claude:" + result.session_id), "sess-abc123");
});

Deno.test("reuses kv-mapped Claude session on follow-up turns", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const kv = makeKv();
    const dicodeId = "abc-123-uuid";
    kv.store.set("claude:" + dicodeId, "sess-pre-existing");

    const result = await withStubClaude(
        `cat <<'JSON'
{"type":"result","is_error":false,"result":"continued","session_id":"sess-pre-existing"}
JSON`,
        () =>
            main({
                params: makeParams([
                    ["prompt", "continue"],
                    ["session_id", dicodeId],
                ]),
                kv, dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, true);
    assertEquals(result.session_id, dicodeId);  // dicode id unchanged
});

Deno.test("rotates Claude session_id mapping when CLI returns a new one", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const kv = makeKv();
    const dicodeId = "rot-uuid-xyz";
    kv.store.set("claude:" + dicodeId, "sess-old");

    await withStubClaude(
        `cat <<'JSON'
{"type":"result","is_error":false,"result":"ok","session_id":"sess-new"}
JSON`,
        () =>
            main({
                params: makeParams([["prompt", "..."], ["session_id", dicodeId]]),
                kv, dicode: fakeDicode,
            }),
    );
    assertEquals(kv.store.get("claude:" + dicodeId), "sess-new");
});

Deno.test("surfaces is_error: true responses", async () => {
    Deno.env.set("CLAUDE_CODE_OAUTH_TOKEN", "stub");
    const result = await withStubClaude(
        `cat <<'JSON'
{"type":"result","is_error":true,"result":"rate limited"}
JSON`,
        () =>
            main({
                params: makeParams([["prompt", "anything"]]),
                kv: makeKv(), dicode: fakeDicode,
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
                params: makeParams([["prompt", "anything"]]),
                kv: makeKv(), dicode: fakeDicode,
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
                params: makeParams([["prompt", "anything"]]),
                kv: makeKv(), dicode: fakeDicode,
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
    const result = await withStubClaude(
        `echo "first: supersecret-token-xyz; second: supersecret-token-xyz" >&2
exit 1`,
        () =>
            main({
                params: makeParams([["prompt", "anything"]]),
                kv: makeKv(), dicode: fakeDicode,
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
    const result = await withStubClaude(
        `echo "non-JSON output mentioning supersecret-token-xyz somehow"`,
        () =>
            main({
                params: makeParams([["prompt", "anything"]]),
                kv: makeKv(), dicode: fakeDicode,
            }),
    );
    assertEquals(result.ok, false);
    if (String(result.error ?? "").includes("supersecret-token-xyz")) {
        throw new Error(`OAuth token leaked via stdout/JSON-parse path: ${result.error}`);
    }
});
