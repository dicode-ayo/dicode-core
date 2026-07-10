/**
 * task.test.ts — unit tests for the notify task's failure-message mapping.
 *
 * Run with:
 *   deno test --allow-all --config=tasks/deno.json tasks/buildin/notify/task.test.ts
 * or:
 *   make test-tasks
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

import { isNoNotificationServer, notifyFailureMessage } from "./failure.ts";

const SERVICE_UNKNOWN =
  "GDBus.Error:org.freedesktop.DBus.Error.ServiceUnknown: The name " +
  "org.freedesktop.Notifications was not provided by any .service files";

test("isNoNotificationServer: true for a GDBus ServiceUnknown on Notifications", () => {
  assert.ok(isNoNotificationServer(SERVICE_UNKNOWN));
});

test("isNoNotificationServer: false for unrelated failures", () => {
  assert.equal(isNoNotificationServer("some other error"), false);
  assert.equal(
    isNoNotificationServer("org.freedesktop.Notifications delivered ok"),
    false,
  );
});

test("notifyFailureMessage: missing server → hint names dunst/mako + README", () => {
  const m = notifyFailureMessage(1, SERVICE_UNKNOWN);
  assert.ok(m.includes("dunst"), "should mention dunst");
  assert.ok(m.includes("mako"), "should mention mako");
  assert.ok(m.includes("README.md"), "should point at the README");
});

test("notifyFailureMessage: other failures pass through with the exit code", () => {
  const m = notifyFailureMessage(2, "boom");
  assert.ok(m.includes("exit 2"));
  assert.ok(m.includes("boom"));
});
