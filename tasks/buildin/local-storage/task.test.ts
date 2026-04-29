import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

test("put + get round-trip", async () => {
  const root = await Deno.makeTempDir();
  const value = btoa("hello world");

  params.set("op", "put");
  params.set("key", "run-inputs/abc");
  params.set("value", value);
  params.set("root", root);
  let res = await runTask() as { ok: boolean };
  assert.equal(res.ok, true);

  params.set("op", "get");
  params.set("key", "run-inputs/abc");
  params.set("value", "");
  params.set("root", root);
  res = await runTask() as { ok: boolean; value: string };
  assert.equal(res.ok, true);
  assert.equal((res as any).value, value);
});

test("get of missing key returns ok with empty value", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "get");
  params.set("key", "run-inputs/missing");
  params.set("root", root);
  const res = await runTask() as { ok: boolean; value: string };
  assert.equal(res.ok, true);
  assert.equal((res as any).value, "");
});

test("delete is idempotent", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "delete");
  params.set("key", "run-inputs/never-existed");
  params.set("root", root);
  const res = await runTask() as { ok: boolean };
  assert.equal(res.ok, true);
});

test("rejects path traversal in key", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "put");
  params.set("key", "run-inputs/../escape");
  params.set("value", btoa("x"));
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("invalid storage key"));
});

test("rejects key without run-inputs/ prefix", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "put");
  params.set("key", "wrong-prefix/abc");
  params.set("value", btoa("x"));
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("must start with"));
});

test("put requires value", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "put");
  params.set("key", "run-inputs/abc");
  params.set("value", "");
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("value required"));
});

test("rejects unknown op", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "frobnicate");
  params.set("key", "run-inputs/abc");
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("unknown op"));
});
