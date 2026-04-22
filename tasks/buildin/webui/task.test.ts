/**
 * task.test.ts — unit tests for the WebUI Example task.
 *
 * Run with:
 *   make test-tasks
 *
 * Each test() block runs in its own isolated runtime. Globals
 * (params, env, log, …) are reset between tests automatically.
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

test("ping returns pong", async () => {
  params.set("action", "ping");

  const result = await runTask();

  assert.equal(result.action, "ping");
  assert.equal(result.pong, true);
  assert.ok(result.timestamp);
});

test("status returns uptime and version", async () => {
  params.set("action", "status");
  env.set("DICODE_VERSION", "1.2.3-test");

  const result = await runTask();

  assert.equal(result.action, "status");
  assert.ok(result.uptime >= 0);
  assert.equal(result.version, "1.2.3-test");
  assert.ok(result.timestamp);
  assert.ok(result.runtime.deno);
  assert.ok(result.runtime.os);
});

test("status uses 'unknown' version when env not set", async () => {
  params.set("action", "status");
  // DICODE_VERSION intentionally not set

  const result = await runTask();

  assert.equal(result.version, "unknown");
});

test("echo returns query back", async () => {
  params.set("action", "echo");
  params.set("query", "hello world");

  const result = await runTask();

  assert.equal(result.action, "echo");
  assert.equal(result.query, "hello world");
  assert.ok(result.timestamp);
});

test("echo throws when query is empty", async () => {
  params.set("action", "echo");
  // query not set — should throw

  await assert.throws(
    () => runTask(),
    /echo action requires a non-empty/,
  );
});

test("unknown action throws", async () => {
  params.set("action", "does-not-exist");

  await assert.throws(
    () => runTask(),
    /Unknown action/,
  );
});

test("ping is default when no action param set", async () => {
  // action not set — defaults to "ping"

  const result = await runTask();

  assert.equal(result.action, "ping");
  assert.equal(result.pong, true);
});
