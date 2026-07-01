import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

test("put + get round-trip", async () => {
  const root = await Deno.makeTempDir();
  const value = btoa("hello world");

  params.set("op", "put");
  params.set("namespace", "my-org/my-task");
  params.set("key", "greeting");
  params.set("value", value);
  params.set("root", root);
  let res = await runTask() as { ok: boolean };
  assert.equal(res.ok, true);

  params.set("op", "get");
  params.set("key", "greeting");
  params.set("value", "");
  res = await runTask() as { ok: boolean; value: string };
  assert.equal(res.ok, true);
  assert.equal((res as any).value, value);
});

test("get of missing key returns ok with empty value", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "get");
  params.set("namespace", "my-org/my-task");
  params.set("key", "missing");
  params.set("root", root);
  const res = await runTask() as { ok: boolean; value: string };
  assert.equal(res.ok, true);
  assert.equal((res as any).value, "");
});

test("delete is idempotent", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "delete");
  params.set("namespace", "my-org/my-task");
  params.set("key", "never-existed");
  params.set("root", root);
  const res = await runTask() as { ok: boolean };
  assert.equal(res.ok, true);
});

test("put then delete then get returns empty", async () => {
  const root = await Deno.makeTempDir();

  params.set("op", "put");
  params.set("namespace", "my-org/my-task");
  params.set("key", "temp");
  params.set("value", btoa("bye"));
  params.set("root", root);
  await runTask();

  params.set("op", "delete");
  params.set("value", "");
  await runTask();

  params.set("op", "get");
  const res = await runTask() as { ok: boolean; value: string };
  assert.equal(res.ok, true);
  assert.equal((res as any).value, "");
});

test("rejects path traversal in key", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "put");
  params.set("namespace", "my-org/my-task");
  params.set("key", "../escape");
  params.set("value", btoa("x"));
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("invalid key"));
});

test("rejects path separators in namespace", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "put");
  params.set("namespace", "../escape");
  params.set("key", "abc");
  params.set("value", btoa("x"));
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("invalid namespace"));
});

test("namespaces are isolated from each other", async () => {
  const root = await Deno.makeTempDir();

  params.set("op", "put");
  params.set("namespace", "team-a");
  params.set("key", "shared-key");
  params.set("value", btoa("team-a value"));
  params.set("root", root);
  await runTask();

  params.set("op", "put");
  params.set("namespace", "team-b");
  params.set("key", "shared-key");
  params.set("value", btoa("team-b value"));
  await runTask();

  params.set("op", "get");
  params.set("namespace", "team-a");
  params.set("value", "");
  let res = await runTask() as { ok: boolean; value: string };
  assert.equal(atob(res.value), "team-a value");

  params.set("op", "get");
  params.set("namespace", "team-b");
  res = await runTask() as { ok: boolean; value: string };
  assert.equal(atob(res.value), "team-b value");
});

test("list returns keys within a namespace", async () => {
  const root = await Deno.makeTempDir();

  for (const key of ["b", "a", "c"]) {
    params.set("op", "put");
    params.set("namespace", "my-org/my-task");
    params.set("key", key);
    params.set("value", btoa(key));
    params.set("root", root);
    await runTask();
  }

  params.set("op", "list");
  params.set("value", "");
  const res = await runTask() as { ok: boolean; keys: string[] };
  assert.equal(res.ok, true);
  assert.equal(JSON.stringify(res.keys), JSON.stringify(["a", "b", "c"]));
});

test("list of namespace with no blobs returns empty array", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "list");
  params.set("namespace", "never-written-to");
  params.set("root", root);
  const res = await runTask() as { ok: boolean; keys: string[] };
  assert.equal(res.ok, true);
  assert.equal(res.keys.length, 0);
});

test("put requires value", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "put");
  params.set("namespace", "my-org/my-task");
  params.set("key", "abc");
  params.set("value", "");
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("value required"));
});

test("put/get/delete require a key", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "put");
  params.set("namespace", "my-org/my-task");
  params.set("value", btoa("x"));
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("key required"));
});

test("rejects unknown op", async () => {
  const root = await Deno.makeTempDir();
  params.set("op", "frobnicate");
  params.set("namespace", "my-org/my-task");
  params.set("key", "abc");
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("unknown op"));
});

test("op is required", async () => {
  const root = await Deno.makeTempDir();
  params.set("root", root);
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(res.error && res.error.includes("op required"));
});
