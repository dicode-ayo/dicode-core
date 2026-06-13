/**
 * task.test.ts — unit tests for audit-export-loki.
 *
 * Run with:  make test-tasks
 *            deno test --allow-read --allow-env --allow-net tasks/buildin/audit-export-loki/task.test.ts
 *
 * freshDicode() has no `audit` sub-object, so each test patches
 * (globalThis as any).dicode.audit before calling runTask() — the same
 * ad-hoc pattern run-inputs-cleanup uses for dicode.runs.
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

// deno-lint-ignore no-explicit-any
type AnyEvent = any;

interface AuditMock {
  // deno-lint-ignore no-explicit-any
  query: (opts: any) => Promise<{ events: AnyEvent[]; next_cursor: string }>;
}

function setAuditMock(mock: AuditMock) {
  // deno-lint-ignore no-explicit-any
  (globalThis as any).dicode.audit = mock;
}

function evt(id: string, over: Partial<AnyEvent> = {}): AnyEvent {
  return {
    id,
    ts: "2026-06-01T12:00:00.000Z",
    event_type: "task_called",
    actor_kind: "task",
    actor_id: "ns/caller",
    target_kind: "task",
    target_id: "ns/target",
    allowed: true,
    ...over,
  };
}

const ENDPOINT = "http://loki.example:3100";

test("ships a batch and advances the cursor on 2xx", async () => {
  let queriedAfter: string | undefined;
  let queriedOrder: string | undefined;
  setAuditMock({
    query: async (opts) => {
      queriedAfter = opts.after;
      queriedOrder = opts.order;
      return { events: [evt("e1"), evt("e2")], next_cursor: "CURSOR-2" };
    },
  });
  http.mock("POST", `${ENDPOINT}/loki/api/v1/push`, { status: 200 });

  params.set("endpoint", ENDPOINT);
  const res = await runTask() as { ok: boolean; shipped: number; cursor: string };

  assert.equal(res.ok, true);
  assert.equal(res.shipped, 2);
  assert.equal(res.cursor, "CURSOR-2");
  // Started from the empty cursor and asked for forward order.
  assert.equal(queriedAfter, "");
  assert.equal(queriedOrder, "asc");
  // Cursor persisted for the next run.
  assert.equal(await kv.get("cursor"), "CURSOR-2");
  assert.httpCalled("POST", `${ENDPOINT}/loki/api/v1/push`);
});

test("resumes from the persisted cursor", async () => {
  let queriedAfter: string | undefined;
  setAuditMock({
    query: async (opts) => {
      queriedAfter = opts.after;
      return { events: [evt("e9")], next_cursor: "CURSOR-9" };
    },
  });
  http.mock("POST", `${ENDPOINT}/loki/api/v1/push`, { status: 200 });

  params.set("endpoint", ENDPOINT);
  kv.set("cursor", "CURSOR-PREV");
  const res = await runTask() as { ok: boolean; shipped: number };
  assert.equal(res.ok, true);
  assert.equal(res.shipped, 1);
  assert.equal(queriedAfter, "CURSOR-PREV");
});

test("no-op when there are no new events", async () => {
  setAuditMock({
    query: async () => ({ events: [], next_cursor: "" }),
  });
  params.set("endpoint", ENDPOINT);
  kv.set("cursor", "CURSOR-KEEP");
  const res = await runTask() as { ok: boolean; shipped: number; cursor: string };
  assert.equal(res.ok, true);
  assert.equal(res.shipped, 0);
  // Cursor unchanged and no push issued.
  assert.equal(res.cursor, "CURSOR-KEEP");
  assert.equal(await kv.get("cursor"), "CURSOR-KEEP");
  assert.httpNotCalled("POST", `${ENDPOINT}/loki/api/v1/push`);
});

test("does NOT advance the cursor when Loki returns non-2xx", async () => {
  setAuditMock({
    query: async () => ({ events: [evt("e1")], next_cursor: "CURSOR-NEW" }),
  });
  http.mock("POST", `${ENDPOINT}/loki/api/v1/push`, { status: 503, body: "overloaded" });

  params.set("endpoint", ENDPOINT);
  kv.set("cursor", "CURSOR-OLD");
  const res = await runTask() as { ok: boolean; shipped: number; status: number };
  assert.equal(res.ok, false);
  assert.equal(res.shipped, 0);
  assert.equal(res.status, 503);
  // Cursor stays put so the batch is retried next tick (at-least-once).
  assert.equal(await kv.get("cursor"), "CURSOR-OLD");
});

test("groups events into streams by low-cardinality labels", async () => {
  setAuditMock({
    query: async () => ({
      events: [
        evt("a", { event_type: "denied", allowed: false }),
        evt("b", { event_type: "denied", allowed: false }),
        evt("c", { event_type: "task_called", allowed: true }),
      ],
      next_cursor: "CURSOR-Z",
    }),
  });
  http.mock("POST", `${ENDPOINT}/loki/api/v1/push`, { status: 200 });

  params.set("endpoint", ENDPOINT);
  await runTask();

  const sent = http.lastRequestBody("POST", `${ENDPOINT}/loki/api/v1/push`) as {
    streams: { stream: Record<string, string>; values: [string, string][] }[];
  };
  // Two distinct (event_type, allowed) tuples → two streams.
  assert.equal(sent.streams.length, 2);
  // Each line value is the JSON-encoded event carrying the id for dedupe.
  const allValues = sent.streams.flatMap((s) => s.values.map((v) => JSON.parse(v[1]).id));
  assert.ok(allValues.includes("a") && allValues.includes("b") && allValues.includes("c"),
    "every event id must appear in a stream line");
});

test("sends Bearer auth when no auth_user is set", async () => {
  setAuditMock({ query: async () => ({ events: [evt("e1")], next_cursor: "C1" }) });
  let seenAuth = "";
  // Capture the header via a custom fetch wrapper since the harness mock
  // records the body but not headers; intercept through a real fetch shim.
  const origFetch = globalThis.fetch;
  (globalThis as unknown as { fetch: typeof fetch }).fetch = (input, init) => {
    seenAuth = (init?.headers as Record<string, string>)?.["authorization"] ?? "";
    return origFetch(input, init);
  };
  http.mock("POST", `${ENDPOINT}/loki/api/v1/push`, { status: 200 });

  params.set("endpoint", ENDPOINT);
  env.set("LOKI_AUTH_TOKEN", "tok-123");
  await runTask();
  (globalThis as unknown as { fetch: typeof fetch }).fetch = origFetch;

  assert.equal(seenAuth, "Bearer tok-123");
});

test("sends Basic auth when auth_user is set", async () => {
  setAuditMock({ query: async () => ({ events: [evt("e1")], next_cursor: "C1" }) });
  let seenAuth = "";
  const origFetch = globalThis.fetch;
  (globalThis as unknown as { fetch: typeof fetch }).fetch = (input, init) => {
    seenAuth = (init?.headers as Record<string, string>)?.["authorization"] ?? "";
    return origFetch(input, init);
  };
  http.mock("POST", `${ENDPOINT}/loki/api/v1/push`, { status: 200 });

  params.set("endpoint", ENDPOINT);
  params.set("auth_user", "12345");
  env.set("LOKI_AUTH_TOKEN", "tok-xyz");
  await runTask();
  (globalThis as unknown as { fetch: typeof fetch }).fetch = origFetch;

  assert.equal(seenAuth, "Basic " + btoa("12345:tok-xyz"));
});

test("fails when endpoint param is missing", async () => {
  setAuditMock({ query: async () => ({ events: [], next_cursor: "" }) });
  // endpoint not set
  const res = await runTask() as { ok: boolean; error: string };
  assert.equal(res.ok, false);
  assert.ok(res.error.includes("endpoint"), `expected endpoint error, got ${res.error}`);
});
