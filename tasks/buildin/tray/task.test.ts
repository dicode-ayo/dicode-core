/**
 * task.test.ts — unit tests for the tray task's SNI visibility hint.
 *
 * Run with:
 *   deno test --allow-all --config=tasks/deno.json tasks/buildin/tray/task.test.ts
 * or:
 *   make test-tasks
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

import { parseHostRegistered, probeSNIHost, trayVisibilityHint } from "./sni.ts";

test("trayVisibilityHint: no hint on macOS/Windows (native tray)", () => {
  assert.equal(trayVisibilityHint("darwin", false), null);
  assert.equal(trayVisibilityHint("windows", false), null);
});

test("trayVisibilityHint: no hint on Linux when a host is registered", () => {
  assert.equal(trayVisibilityHint("linux", true), null);
});

test("trayVisibilityHint: no hint on Linux when the probe was inconclusive", () => {
  assert.equal(trayVisibilityHint("linux", null), null);
});

test("trayVisibilityHint: Linux + no host → actionable hint", () => {
  const h = trayVisibilityHint("linux", false);
  assert.ok(h, "expected a hint string");
  assert.ok(h!.includes("snixembed"), "hint should name a bridge");
  assert.ok(h!.includes("README.md"), "hint should point at the README");
});

test("parseHostRegistered: reads gdbus true/false, null on garbage", () => {
  assert.equal(parseHostRegistered("(<true>,)\n"), true);
  assert.equal(parseHostRegistered("(<false>,)\n"), false);
  assert.equal(parseHostRegistered("unexpected reply"), null);
});

test("probeSNIHost: false when no watcher name is registered", async () => {
  assert.equal(await probeSNIHost(() => Promise.resolve(null)), false);
});

test("probeSNIHost: true when a watcher reports a host", async () => {
  assert.equal(await probeSNIHost(() => Promise.resolve("(<true>,)")), true);
});

test("probeSNIHost: null when the runner throws (tooling missing)", async () => {
  assert.equal(
    await probeSNIHost(() => Promise.reject(new Error("no gdbus"))),
    null,
  );
});
