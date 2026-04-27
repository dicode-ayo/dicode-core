/**
 * task.test.ts — unit tests for the Auth Providers dashboard task.
 *
 * Run with:  make test-tasks
 *
 * Each test() runs in its own isolated runtime; mocks (params, env,
 * dicode.*) reset between tests automatically.
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

test("list action returns merged status + meta for each provider", async () => {
  params.set("providers", "github,openrouter");

  let calledWith: string[] | null = null;
  dicode.oauth = {
    list_status: async (arr: string[]) => {
      calledWith = arr;
      return [
        { provider: "github",     has_token: true,  expires_at: "2026-12-31T00:00:00Z", scope: "user repo" },
        { provider: "openrouter", has_token: false },
      ];
    },
  };

  const result = await runTask() as Array<Record<string, unknown>>;

  assert.equal(JSON.stringify(calledWith), JSON.stringify(["github", "openrouter"]));
  assert.equal(result.length, 2);
  assert.equal(result[0].provider, "github");
  assert.equal(result[0].has_token, true);
  const meta0 = result[0].meta as Record<string, unknown>;
  assert.equal(meta0.label, "GitHub");
  const meta1 = result[1].meta as Record<string, unknown>;
  assert.equal(meta1.label, "OpenRouter");
  assert.ok((meta1.standalone as Record<string, unknown>)?.webhookPath);
});

test("connect for a relay-broker provider calls auth-start and returns its url", async () => {
  params.set("providers", "github");
  globalThis.input = { action: "connect", provider: "github" };

  const runTaskCalls: Array<{ id: string; params?: Record<string, string> }> = [];
  dicode.run_task = async (id: string, p?: Record<string, string>) => {
    runTaskCalls.push({ id, params: p });
    return { returnValue: { url: "https://relay.example/auth/github?...", session_id: "sess-1" } };
  };

  const result = await runTask() as Record<string, unknown>;

  assert.equal(runTaskCalls.length, 1);
  assert.equal(runTaskCalls[0].id, "buildin/auth-start");
  assert.equal(runTaskCalls[0].params?.provider, "github");
  assert.equal(result.provider, "github");
  assert.equal(result.url, "https://relay.example/auth/github?...");
  assert.equal(result.session_id, "sess-1");
});

test("connect for openrouter (standalone) does NOT call run_task", async () => {
  params.set("providers", "openrouter");
  globalThis.input = { action: "connect", provider: "openrouter" };
  env.set("DICODE_BASE_URL", "http://localhost:8080");

  let runTaskCalls = 0;
  dicode.run_task = async () => { runTaskCalls += 1; return {}; };

  const result = await runTask() as Record<string, unknown>;

  assert.equal(runTaskCalls, 0);
  assert.equal(result.provider, "openrouter");
  assert.equal(result.url, "http://localhost:8080/hooks/openrouter-oauth");
});

test("connect for an unknown provider throws", async () => {
  params.set("providers", "github");
  globalThis.input = { action: "connect", provider: "no-such-provider" };

  await assert.throws(() => runTask(), /unknown provider/);
});

test("connect when auth-start returns no url throws", async () => {
  params.set("providers", "github");
  globalThis.input = { action: "connect", provider: "github" };
  dicode.run_task = async () => ({ returnValue: {} });

  await assert.throws(() => runTask(), /did not return a url/);
});

test("empty providers param yields empty list (no list_status call)", async () => {
  params.set("providers", "");
  let called = 0;
  dicode.oauth = {
    list_status: async () => { called += 1; return []; },
  };

  const result = await runTask() as unknown[];

  assert.equal(called, 0);
  assert.equal(result.length, 0);
});

test("more than 64 providers throws before any IPC call", async () => {
  const big = Array.from({ length: 65 }, (_, i) => `p${i}`).join(",");
  params.set("providers", big);
  let called = 0;
  dicode.oauth = {
    list_status: async () => { called += 1; return []; },
  };

  await assert.throws(() => runTask(), /at most 64 providers/);
  assert.equal(called, 0);
});
