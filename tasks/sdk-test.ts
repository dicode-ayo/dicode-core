import { parse as parseYaml } from "jsr:@std/yaml@1";

/**
 * sdk-test.ts — Deno test harness for `tasks/buildin/*\/task.test.ts`.
 *
 * The production Deno SDK (pkg/runtime/deno/sdk/shim.ts) bridges globals
 * over a Unix socket to the daemon. Task tests need the same surface but
 * with pure in-memory mocks — no daemon, no sockets. This file provides:
 *
 *   • `test(name, fn)`       — registers a Deno.test that resets mocks first
 *   • `params`/`env`/`kv`    — in-memory get/set/delete/list
 *   • `http.mock/mockOnce`   — fetch interceptors keyed on (method, pattern)
 *   • `http.lastRequestBody` — retrieve the last captured body for a call
 *   • `assert.*`             — equal/ok/throws + http-specific helpers
 *   • `runTask()`            — imports the adjacent task.ts and calls main()
 *
 * Usage in a task.test.ts:
 *
 *   import { setupHarness } from "../../sdk-test.ts";
 *   await setupHarness(import.meta.url);
 *
 *   test("ping returns pong", async () => {
 *     params.set("action", "ping");
 *     const result = await runTask();
 *     assert.equal(result.pong, true);
 *   });
 */

type AnyFn = (...args: unknown[]) => unknown;

// ─── mock state ──────────────────────────────────────────────────────────

interface MockHttpResponse {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
}

interface HttpMock {
  method: string;
  pattern: string;
  response: MockHttpResponse;
  oneShot: boolean;
  consumed: boolean;
}

interface HttpCall {
  method: string;
  url: string;
  body?: unknown;
}

// deno-lint-ignore no-explicit-any
type MockDicode = Record<string, any>;

interface State {
  params: Map<string, string>;
  paramDefaults: Map<string, string>;
  env: Map<string, string>;
  kv: Map<string, unknown>;
  httpMocks: HttpMock[];
  httpCalls: HttpCall[];
  taskModuleUrl: string;
  dicode: MockDicode;
  input: unknown;
}

function freshDicode(): MockDicode {
  return {
    task_id: "test/task",
    run_id: "test-run",
    run_task: async () => ({}),
    list_tasks: async () => [],
    get_runs: async () => [],
    secrets_set: async () => {},
    secrets_delete: async () => {},
  };
}

const state: State = {
  params: new Map(),
  paramDefaults: new Map(),
  env: new Map(),
  kv: new Map(),
  httpMocks: [],
  httpCalls: [],
  taskModuleUrl: "",
  dicode: freshDicode(),
  input: undefined,
};

function resetMocks() {
  state.params.clear();
  // Re-seed with task.yaml defaults so tasks that read required-with-default
  // params don't have to be set explicitly by every single test.
  for (const [k, v] of state.paramDefaults) state.params.set(k, v);
  state.env.clear();
  state.kv.clear();
  state.httpMocks = [];
  state.httpCalls = [];
  state.dicode = freshDicode();
  state.input = undefined;
  // Re-expose the fresh mutable object under globalThis.dicode so tests
  // picking it up after resetMocks see the cleared defaults.
  (globalThis as unknown as { dicode: MockDicode; input: unknown }).dicode = state.dicode;
  (globalThis as unknown as { input: unknown }).input = undefined;
}

// ─── SDK mocks (shape matches DicodeSdk in tasks/sdk.ts) ─────────────────

interface MockParams {
  set(k: string, v: string): void;
  get(k: string): Promise<string | null>;
  all(): Promise<Record<string, string>>;
}

interface MockEnv {
  set(k: string, v: string): void;
}

interface MockKV {
  set(k: string, v: unknown): void;
  get(k: string): Promise<unknown>;
  delete(k: string): Promise<void>;
  list(prefix?: string): Promise<Record<string, unknown>>;
}

const params: MockParams = {
  set(k, v) { state.params.set(k, v); },
  // eslint-disable-next-line @typescript-eslint/require-await
  async get(k) { return state.params.get(k) ?? null; },
  // eslint-disable-next-line @typescript-eslint/require-await
  async all() { return Object.fromEntries(state.params); },
};

const env: MockEnv = {
  set(k, v) { state.env.set(k, v); },
};

const kv: MockKV = {
  set(k, v) { state.kv.set(k, v); },
  // eslint-disable-next-line @typescript-eslint/require-await
  async get(k) { return state.kv.get(k); },
  // eslint-disable-next-line @typescript-eslint/require-await
  async delete(k) { state.kv.delete(k); },
  // eslint-disable-next-line @typescript-eslint/require-await
  async list(prefix = "") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of state.kv) {
      if (k.startsWith(prefix)) out[k] = v;
    }
    return out;
  },
};

function globMatch(pattern: string, url: string): boolean {
  if (pattern === url) return true;
  if (!pattern.includes("*")) return false;
  const re = "^" + pattern.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*") + "$";
  return new RegExp(re).test(url);
}

interface MockHttp {
  mock(method: string, pattern: string, response: MockHttpResponse): void;
  mockOnce(method: string, pattern: string, response: MockHttpResponse): void;
  // lastRequestBody returns `any` so tests can drill into task-specific
  // request shapes without a per-task type annotation.
  // deno-lint-ignore no-explicit-any
  lastRequestBody(method: string, pattern: string): any;
}

const http: MockHttp = {
  mock(method, pattern, response) {
    state.httpMocks.push({ method, pattern, response, oneShot: false, consumed: false });
  },
  mockOnce(method, pattern, response) {
    state.httpMocks.push({ method, pattern, response, oneShot: true, consumed: false });
  },
  lastRequestBody(method, pattern) {
    for (let i = state.httpCalls.length - 1; i >= 0; i--) {
      const c = state.httpCalls[i];
      if (c.method === method && globMatch(pattern, c.url)) return c.body;
    }
    return undefined;
  },
};

// ─── assert ──────────────────────────────────────────────────────────────

function deepEq(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a == null || b == null) return false;
  if (typeof a !== typeof b) return false;
  if (typeof a !== "object") return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  const ka = Object.keys(a as object);
  const kb = Object.keys(b as object);
  if (ka.length !== kb.length) return false;
  for (const k of ka) {
    if (!deepEq((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k])) return false;
  }
  return true;
}

interface MockAssert {
  equal(a: unknown, b: unknown, msg?: string): void;
  ok(v: unknown, msg?: string): void;
  throws(fn: AnyFn, pattern?: RegExp | string): Promise<void>;
  httpCalled(method: string, pattern: string): void;
  httpNotCalled(method: string, pattern: string): void;
  httpCalledWith(method: string, url: string, opts: { body?: unknown }): void;
}

const assert: MockAssert = {
  equal(a, b, msg) {
    if (!deepEq(a, b)) throw new Error(msg ?? `assert.equal: ${JSON.stringify(a)} !== ${JSON.stringify(b)}`);
  },
  ok(v, msg) {
    if (!v) throw new Error(msg ?? `assert.ok: got ${JSON.stringify(v)}`);
  },
  async throws(fn, pattern) {
    let thrown: unknown = undefined;
    try {
      await fn();
    } catch (e) {
      thrown = e;
    }
    if (thrown === undefined) throw new Error("assert.throws: function did not throw");
    if (pattern) {
      const msg = (thrown as Error)?.message ?? String(thrown);
      const re = pattern instanceof RegExp ? pattern : new RegExp(pattern);
      if (!re.test(msg)) throw new Error(`assert.throws: thrown ${JSON.stringify(msg)} does not match ${pattern}`);
    }
  },
  httpCalled(method, pattern) {
    const hit = state.httpCalls.some((c) => c.method === method && globMatch(pattern, c.url));
    if (!hit) throw new Error(`assert.httpCalled: no ${method} ${pattern} in ${JSON.stringify(state.httpCalls.map((c) => c.method + " " + c.url))}`);
  },
  httpNotCalled(method, pattern) {
    const hit = state.httpCalls.some((c) => c.method === method && globMatch(pattern, c.url));
    if (hit) throw new Error(`assert.httpNotCalled: unexpected ${method} ${pattern}`);
  },
  httpCalledWith(method, url, opts) {
    const match = state.httpCalls.find((c) => c.method === method && globMatch(url, c.url));
    if (!match) throw new Error(`assert.httpCalledWith: no ${method} ${url}`);
    if (opts.body !== undefined && !deepEq(match.body, opts.body)) {
      throw new Error(`assert.httpCalledWith: body ${JSON.stringify(match.body)} !== ${JSON.stringify(opts.body)}`);
    }
  },
};

// ─── fetch interceptor ───────────────────────────────────────────────────

const realFetch = globalThis.fetch;

async function mockedFetch(
  input: string | URL | Request,
  init?: RequestInit,
): Promise<Response> {
  const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
  const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();

  let body: unknown = undefined;
  if (init?.body && typeof init.body === "string") {
    try { body = JSON.parse(init.body); } catch { body = init.body; }
  } else if (input instanceof Request) {
    try { body = await input.clone().json(); } catch { /* not json */ }
  }

  state.httpCalls.push({ method, url, body });

  for (const m of state.httpMocks) {
    if (m.consumed) continue;
    if (m.method !== method) continue;
    if (!globMatch(m.pattern, url)) continue;
    if (m.oneShot) m.consumed = true;

    const responseBody = typeof m.response.body === "string"
      ? m.response.body
      : JSON.stringify(m.response.body ?? null);
    return new Response(responseBody, {
      status: m.response.status,
      headers: { "content-type": "application/json", ...(m.response.headers ?? {}) },
    });
  }

  // No mock matched — either fail loudly (typical) or fall through to real
  // fetch (for tests that want real-network behaviour). We fail loudly.
  throw new Error(`[sdk-test] no mock matches ${method} ${url} (httpCalls=${state.httpCalls.length})`);
}

// ─── harness setup ───────────────────────────────────────────────────────

let taskMain: ((sdk: unknown) => unknown) | null = null;

// runTask returns `any` because test code inherently reaches into
// task-specific result shapes (`result.pong`, `result.uptime.deno`, etc.)
// and typing each one here would require the harness to know every task.
// Tests opt into the dynamic nature of the tasks they cover.
// deno-lint-ignore no-explicit-any
async function runTask(): Promise<any> {
  if (!taskMain) throw new Error("runTask: setupHarness(import.meta.url) must be awaited before test() runs");
  // Pull the freshest globalThis.input so tests can assign it directly
  // without going through a setter — matches the documented harness API.
  const liveInput = (globalThis as unknown as { input: unknown }).input;
  const sdk = {
    params,
    kv,
    input: liveInput ?? state.input,
    output: { html: async () => {}, text: async () => {}, image: async () => {}, file: async () => {} },
    mcp: { list_tools: async () => [], call: async () => ({}) },
    dicode: state.dicode,
  };
  return (await taskMain(sdk)) as Record<string, unknown>;
}

/**
 * setupHarness wires in mocks and dynamically imports the task module
 * at ./task.ts relative to the caller. Must be awaited once at the top of
 * a task.test.ts BEFORE any test() calls.
 */
export async function setupHarness(testFileUrl: string): Promise<void> {
  state.taskModuleUrl = new URL("./task.ts", testFileUrl).toString();

  // Parse the adjacent task.yaml and collect param defaults so resetMocks
  // can seed them before each test() runs — matches what the production
  // daemon does on run-time parameter resolution.
  try {
    const yamlUrl = new URL("./task.yaml", testFileUrl);
    const yamlText = await Deno.readTextFile(yamlUrl);
    const spec = parseYaml(yamlText) as { params?: Record<string, { default?: unknown }> };
    if (spec?.params) {
      state.paramDefaults.clear();
      for (const [name, def] of Object.entries(spec.params)) {
        if (def && typeof def === "object" && "default" in def && def.default !== undefined) {
          state.paramDefaults.set(name, String(def.default));
        }
      }
    }
  } catch (e) {
    console.warn(`[sdk-test] could not load task.yaml defaults: ${(e as Error).message}`);
  }

  // Intercept Deno.env.get — production tasks read version etc. this way.
  const origEnvGet = Deno.env.get.bind(Deno.env);
  (Deno.env as unknown as { get: (k: string) => string | undefined }).get = (k: string) =>
    state.env.get(k) ?? origEnvGet(k);

  // Intercept fetch so http.mock actually bites.
  (globalThis as unknown as { fetch: typeof fetch }).fetch = mockedFetch;

  // Expose globals so task.test.ts can call them without import noise.
  Object.assign(globalThis, {
    test: (name: string, fn: () => void | Promise<void>) => {
      Deno.test(name, async () => {
        resetMocks();
        await fn();
      });
    },
    params,
    env,
    kv,
    http,
    assert,
    runTask,
    dicode: state.dicode,
    input: undefined,
  });

  const mod = await import(state.taskModuleUrl);
  taskMain = mod.default as (sdk: unknown) => unknown;
  if (typeof taskMain !== "function") {
    throw new Error(`setupHarness: ${state.taskModuleUrl} has no default export function`);
  }
}

// Type declarations for the injected globals so task.test.ts passes type
// checks under Deno's strict mode. The values are set by setupHarness.
declare global {
  // deno-lint-ignore no-var
  var test: (name: string, fn: () => void | Promise<void>) => void;
  // deno-lint-ignore no-var
  var params: MockParams;
  // deno-lint-ignore no-var
  var env: MockEnv;
  // deno-lint-ignore no-var
  var kv: MockKV;
  // deno-lint-ignore no-var
  var http: MockHttp;
  // deno-lint-ignore no-var
  var assert: MockAssert;
  // deno-lint-ignore no-var no-explicit-any
  var runTask: () => Promise<any>;
  // deno-lint-ignore no-var
  var dicode: MockDicode;
  // deno-lint-ignore no-var
  var input: unknown;
}

// Re-export realFetch so tests that explicitly want real network can grab it.
export { realFetch };
