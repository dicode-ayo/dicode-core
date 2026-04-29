// task.test.ts — Doppler provider with mocked fetch.
//
// The shared sdk-test.ts harness builds a non-callable `output` object,
// but provider tasks expect `output(map, { secret: true })`. We bypass
// the harness here and stand up minimal SDK mocks per test.

import { assertEquals, assertRejects } from "jsr:@std/assert@1";
import main from "./task.ts";

interface MockSdk {
  params: { get: (k: string) => Promise<string | null>; all: () => Promise<Record<string, string>> };
  output: ReturnType<typeof makeOutput>;
  kv: { get: (k: string) => Promise<unknown>; set: () => Promise<void>; delete: () => Promise<void>; list: () => Promise<Record<string, unknown>> };
  input: unknown;
  mcp: { list_tools: () => Promise<unknown[]>; call: () => Promise<unknown> };
  dicode: Record<string, unknown>;
}

function makeOutput() {
  const calls: { value: Record<string, string>; opts: { secret: true } }[] = [];
  // deno-lint-ignore no-explicit-any
  const fn: any = async (value: Record<string, string>, opts: { secret: true }) => {
    calls.push({ value, opts });
  };
  fn.html = async () => {};
  fn.text = async () => {};
  fn.image = async () => {};
  fn.file = async () => {};
  fn.calls = calls;
  return fn;
}

function makeSdk(requests: unknown): MockSdk {
  return {
    params: {
      get: async (k: string) => k === "requests" ? JSON.stringify(requests) : null,
      all: async () => ({}),
    },
    output: makeOutput(),
    kv: { get: async () => undefined, set: async () => {}, delete: async () => {}, list: async () => ({}) },
    input: undefined,
    mcp: { list_tools: async () => [], call: async () => ({}) },
    dicode: {},
  };
}

Deno.test("Doppler provider returns requested secrets", async () => {
  Deno.env.set("DOPPLER_TOKEN", "dp.st.test");
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (input: string | URL | Request, _init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
    if (url.startsWith("https://api.doppler.com/v3/configs/config/secrets")) {
      return Promise.resolve(new Response(JSON.stringify({
        secrets: {
          PG_URL: { computed: "postgres://example.com/db" },
          REDIS_URL: { computed: "redis://example.com:6379" },
        },
      }), { status: 200 }));
    }
    return Promise.reject(new Error("unexpected fetch: " + url));
  };

  const sdk = makeSdk([
    { name: "PG_URL", optional: false },
    { name: "REDIS_URL", optional: true },
    { name: "MISSING_OPT", optional: true },
  ]);

  try {
    // deno-lint-ignore no-explicit-any
    await main(sdk as any);
  } finally {
    globalThis.fetch = originalFetch;
  }

  // deno-lint-ignore no-explicit-any
  const calls = (sdk.output as any).calls as { value: Record<string, string>; opts: { secret: true } }[];
  assertEquals(calls.length, 1);
  assertEquals(calls[0].opts.secret, true);
  assertEquals(calls[0].value["PG_URL"], "postgres://example.com/db");
  assertEquals(calls[0].value["REDIS_URL"], "redis://example.com:6379");
  assertEquals("MISSING_OPT" in calls[0].value, false);
});

Deno.test("Doppler provider accepts a bare-array requests payload", async () => {
  Deno.env.set("DOPPLER_TOKEN", "dp.st.test");
  const originalFetch = globalThis.fetch;
  globalThis.fetch = () => Promise.resolve(new Response(JSON.stringify({
    secrets: { FOO: { computed: "bar" } },
  }), { status: 200 }));

  // Bare array shape — what the resolver doc says.
  const sdk = makeSdk([{ name: "FOO", optional: false }]);

  try {
    // deno-lint-ignore no-explicit-any
    await main(sdk as any);
  } finally {
    globalThis.fetch = originalFetch;
  }

  // deno-lint-ignore no-explicit-any
  const calls = (sdk.output as any).calls as { value: Record<string, string> }[];
  assertEquals(calls[0].value["FOO"], "bar");
});

Deno.test("Doppler provider throws on required miss", async () => {
  Deno.env.set("DOPPLER_TOKEN", "dp.st.test");
  const originalFetch = globalThis.fetch;
  globalThis.fetch = () => Promise.resolve(new Response(JSON.stringify({
    secrets: {},
  }), { status: 200 }));

  const sdk = makeSdk([{ name: "PG_URL", optional: false }]);

  try {
    await assertRejects(
      // deno-lint-ignore no-explicit-any
      () => main(sdk as any),
      Error,
      "required secret PG_URL",
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});

Deno.test("Doppler provider throws when DOPPLER_TOKEN is missing", async () => {
  Deno.env.delete("DOPPLER_TOKEN");
  const sdk = makeSdk([]);
  await assertRejects(
    // deno-lint-ignore no-explicit-any
    () => main(sdk as any),
    Error,
    "DOPPLER_TOKEN not set",
  );
});

Deno.test("Doppler provider surfaces non-2xx Doppler API responses", async () => {
  Deno.env.set("DOPPLER_TOKEN", "dp.st.test");
  const originalFetch = globalThis.fetch;
  globalThis.fetch = () => Promise.resolve(new Response("forbidden", { status: 403 }));

  const sdk = makeSdk([{ name: "X", optional: true }]);

  try {
    await assertRejects(
      // deno-lint-ignore no-explicit-any
      () => main(sdk as any),
      Error,
      "Doppler API 403",
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});
