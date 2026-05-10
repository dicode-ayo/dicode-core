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

const BROKER_URL = "https://relay.example";
const BROKER_PROVIDERS = [
  { key: "github", pkce: true, scopes: ["user", "repo"], secret_required: true,  configured: true },
  { key: "slack",  pkce: true, scopes: ["channels:read"], secret_required: false, configured: true },
];

function mockBrokerProviders(providers: unknown = BROKER_PROVIDERS): void {
  env.set("DICODE_RELAY_BROKER_URL", BROKER_URL);
  http.mock("GET", `${BROKER_URL}/providers`, {
    status: 200,
    body: providers,
  });
}

function withSecretsHas(present: boolean): void {
  dicode.secrets = { has: async () => present };
}

test("list with broker returns the broker catalogue + openrouter standalone", async () => {
  mockBrokerProviders();
  withSecretsHas(false);

  const result = await runTask() as Array<Record<string, unknown>>;

  // Broker entries first, then the openrouter standalone.
  assert.equal(result.length, 3);
  assert.equal(result[0].key, "github");
  assert.equal(result[0].has_token, false);
  assert.equal(result[1].key, "slack");
  assert.equal(result[2].key, "openrouter");
  assert.ok((result[2].standalone as Record<string, unknown>)?.webhookPath);
});

test("list without broker returns only standalones (BYO without relay)", async () => {
  // No DICODE_RELAY_BROKER_URL → broker call is skipped entirely.
  withSecretsHas(false);

  const result = await runTask() as Array<Record<string, unknown>>;

  assert.equal(result.length, 1);
  assert.equal(result[0].key, "openrouter");
  assert.ok((result[0].standalone as Record<string, unknown>)?.webhookPath);
});

test("list filters by `providers` param when set", async () => {
  mockBrokerProviders();
  params.set("providers", "github,openrouter");
  withSecretsHas(false);

  const result = await runTask() as Array<Record<string, unknown>>;

  assert.equal(result.length, 2);
  assert.equal(result.map(r => r.key).sort(), ["github", "openrouter"]);
});

test("list reports has_token=true when secrets.has returns true for a key", async () => {
  mockBrokerProviders([
    { key: "github", pkce: true, scopes: [], secret_required: false, configured: true },
  ]);
  // Only GITHUB_ACCESS_TOKEN is "stored".
  dicode.secrets = {
    has: async (key: string) => key === "GITHUB_ACCESS_TOKEN",
  };

  const result = await runTask() as Array<Record<string, unknown>>;

  const github = result.find(r => r.key === "github");
  const openrouter = result.find(r => r.key === "openrouter");
  assert.equal(github?.has_token, true);
  assert.equal(openrouter?.has_token, false);
});

test("connect for openrouter (standalone) returns the webhook URL without run_task", async () => {
  globalThis.input = { action: "connect", provider: "openrouter" };
  env.set("DICODE_BASE_URL", "http://localhost:8080");

  let runTaskCalls = 0;
  dicode.run_task = async () => { runTaskCalls += 1; return {}; };

  const result = await runTask() as Record<string, unknown>;

  assert.equal(runTaskCalls, 0);
  assert.equal(result.provider, "openrouter");
  assert.equal(result.url, "http://localhost:8080/hooks/openrouter-oauth");
});

test("connect for a broker provider with relay enabled delegates to auth-start", async () => {
  globalThis.input = { action: "connect", provider: "github" };
  env.set("DICODE_RELAY_BROKER_URL", BROKER_URL);

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

test("connect for a broker provider when relay is off throws", async () => {
  // No DICODE_RELAY_BROKER_URL set.
  globalThis.input = { action: "connect", provider: "github" };

  await assert.throws(() => runTask(), /requires the relay broker/);
});

test("connect when auth-start returns no url throws", async () => {
  globalThis.input = { action: "connect", provider: "github" };
  env.set("DICODE_RELAY_BROKER_URL", BROKER_URL);
  dicode.run_task = async () => ({ returnValue: {} });

  await assert.throws(() => runTask(), /did not return a url/);
});

test("unknown action throws", async () => {
  globalThis.input = { action: "no-such-action" };

  await assert.throws(() => runTask(), /unknown action/);
});
