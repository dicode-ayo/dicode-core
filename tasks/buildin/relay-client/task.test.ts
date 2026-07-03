/**
 * task.test.ts — unit tests for buildin/relay-client.
 *
 * Covers the scoping predicate for issue #460: when the relay broker is
 * unreachable, npm ws@8.x can throw a TypeError from an AbortSignal timeout
 * callback (abortHandshake on an already-nulled ClientRequest) OUTSIDE the
 * promise chain the task awaits. isAbortedHandshakeFault() decides which
 * uncaught errors the process-level handler converts into a reconnect —
 * everything else must stay fatal so real bugs aren't masked.
 *
 * main() is an infinite daemon loop, so these tests exercise the exported
 * predicate directly rather than going through the sdk-test harness's
 * runTask(). Deliberately dependency-free (no jsr imports): plain Deno.test
 * with an inline assert so the file runs without registry access.
 */

import { isAbortedHandshakeFault } from "./task.ts";

function assertEq(got: unknown, want: unknown, msg: string): void {
  if (got !== want) {
    throw new Error(`${msg}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
  }
}

// Fabricates the fault shape from issue #460: a TypeError whose stack ends
// in ws's handshake path, thrown from an AbortSignal timeout callback.
function wsFault(message: string, stack: string): TypeError {
  const err = new TypeError(message);
  err.stack = `TypeError: ${message}\n${stack}`;
  return err;
}

const ABORT_HANDSHAKE_STACK = [
  "    at abortHandshake (file:///data/node_modules/.deno/ws@8.20.0/node_modules/ws/lib/websocket.js:1094:14)",
  "    at ClientRequest.<anonymous> (file:///data/node_modules/.deno/ws@8.20.0/node_modules/ws/lib/websocket.js:878:7)",
  "    at AbortSignal._timeoutCb (node:http:695:32)",
].join("\n");

Deno.test("swallows the issue #460 fault: abortHandshake null setHeader deref", () => {
  const err = wsFault(
    "Cannot read properties of null (reading 'setHeader')",
    ABORT_HANDSHAKE_STACK,
  );
  assertEq(isAbortedHandshakeFault(err), true, "abortHandshake null-deref");
});

Deno.test("swallows a ws handshake null-deref without an abortHandshake frame", () => {
  // Error.captureStackTrace can trim the abortHandshake frame; the
  // ws/lib/websocket.js path alone still identifies the handshake path.
  const err = wsFault(
    "Cannot read properties of null (reading 'socket')",
    "    at ClientRequest.<anonymous> (node_modules/ws/lib/websocket.js:878:7)\n" +
      "    at AbortSignal._timeoutCb (node:http:695:32)",
  );
  assertEq(isAbortedHandshakeFault(err), true, "ws websocket.js null-deref");
});

Deno.test("swallows the older V8 null-deref phrasing", () => {
  const err = wsFault(
    "Cannot read property 'setHeader' of null",
    ABORT_HANDSHAKE_STACK,
  );
  assertEq(isAbortedHandshakeFault(err), true, "legacy V8 phrasing");
});

Deno.test("does not swallow the same message from non-ws code", () => {
  const err = wsFault(
    "Cannot read properties of null (reading 'setHeader')",
    "    at runOnce (file:///data/tasks/buildin/relay-client/task.ts:98:3)",
  );
  assertEq(isAbortedHandshakeFault(err), false, "non-ws stack must stay fatal");
});

Deno.test("does not swallow non-TypeError handshake errors", () => {
  // "Opening handshake has timed out" is emitted on the websocket's normal
  // error path — RelayClient's own backoff handles it; the process-level
  // handler must not.
  const err = new Error("Opening handshake has timed out");
  err.stack = `Error: Opening handshake has timed out\n${ABORT_HANDSHAKE_STACK}`;
  assertEq(isAbortedHandshakeFault(err), false, "plain Error must stay fatal");
});

Deno.test("does not swallow other TypeErrors from ws internals", () => {
  const err = wsFault(
    "Cannot read properties of undefined (reading 'send')",
    ABORT_HANDSHAKE_STACK,
  );
  assertEq(isAbortedHandshakeFault(err), false, "undefined-deref must stay fatal");
});

Deno.test("does not swallow non-Error values", () => {
  assertEq(
    isAbortedHandshakeFault("Cannot read properties of null (reading 'setHeader')"),
    false,
    "string reason must stay fatal",
  );
  assertEq(isAbortedHandshakeFault(null), false, "null must stay fatal");
  assertEq(isAbortedHandshakeFault(undefined), false, "undefined must stay fatal");
});
