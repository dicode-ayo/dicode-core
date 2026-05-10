/**
 * task.test.ts — unit tests for buildin/relay-server.
 *
 * All three cases below need to import ./task.ts, which transitively imports
 * `npm:dicode-relay@^0.2/start`. That entry point ships in dicode-relay 0.2.0,
 * tracked in dicode-ayo/dicode-relay#70 — until 0.2.0 is on npm the dynamic
 * import fails before any assertion runs.
 *
 * Strategy: gate every dynamic import behind `RELAY_PRERELEASE_PATH`. When
 * unset (the default in CI today), each test logs a skip-and-pass note.
 * When set to a local dicode-relay checkout's `src/start.ts`, the dynamic
 * import resolves via Deno's npm shim against that path and the cases run
 * end-to-end.
 *
 *   RELAY_PRERELEASE_PATH=/abs/path/to/dicode-relay deno test --allow-all \
 *     tasks/buildin/relay-server/task.test.ts
 *
 * TODO(dicode-relay#70): once 0.2.0 is published, drop the gate and let the
 * cases run unconditionally — the structure of each case is already
 * production-shaped.
 *
 * The harness in tasks/sdk-test.ts dynamically imports task.ts at
 * setupHarness time, so we cannot use it here; instead we hand-roll
 * minimal SDK mocks per test (same approach as the doppler provider).
 */

import { assertRejects } from "jsr:@std/assert@1";

const RELAY_PRERELEASE_PATH = Deno.env.get("RELAY_PRERELEASE_PATH");
const PRERELEASE_SKIP_MSG =
  "skipped: set RELAY_PRERELEASE_PATH=<path-to-dicode-relay checkout> " +
  "to run (gated on dicode-relay#70 publishing 0.2.0)";

// Doppler-fed env vars that the daemon would otherwise populate. We clear
// all of them before each test so fixtures are deterministic regardless of
// the developer's shell env.
const RELAY_ENV_VARS = [
  "STATUS_PASSWORD",
  "GITHUB_CLIENT_ID",
  "GITHUB_CLIENT_SECRET",
  "SLACK_CLIENT_ID",
  "GOOGLE_CLIENT_ID",
  "GOOGLE_CLIENT_SECRET",
  "SPOTIFY_CLIENT_ID",
  "LINEAR_CLIENT_ID",
  "DISCORD_CLIENT_ID",
  "GITLAB_CLIENT_ID",
  "GITLAB_CLIENT_SECRET",
  "AIRTABLE_CLIENT_ID",
  "AIRTABLE_CLIENT_SECRET",
  "NOTION_CLIENT_ID",
  "NOTION_CLIENT_SECRET",
  "CONFLUENCE_CLIENT_ID",
  "SALESFORCE_CLIENT_ID",
  "STRIPE_CLIENT_ID",
  "STRIPE_CLIENT_SECRET",
  "OFFICE365_CLIENT_ID",
  "OFFICE365_CLIENT_SECRET",
  "AZURE_CLIENT_ID",
  "AZURE_CLIENT_SECRET",
  "BASE_URL",
];

function clearRelayEnv(): void {
  for (const k of RELAY_ENV_VARS) Deno.env.delete(k);
}

interface MockSdk {
  params: {
    get: (k: string) => Promise<string | null>;
    all: () => Promise<Record<string, string>>;
  };
  kv: {
    get: () => Promise<unknown>;
    set: () => Promise<void>;
    delete: () => Promise<void>;
    list: () => Promise<Record<string, unknown>>;
  };
  input: unknown;
  output: () => Promise<void>;
  mcp: { list_tools: () => Promise<unknown[]>; call: () => Promise<unknown> };
  dicode: Record<string, unknown>;
}

function makeSdk(paramValues: Record<string, string>): MockSdk {
  return {
    params: {
      get: (k: string) => Promise.resolve(paramValues[k] ?? null),
      all: () => Promise.resolve({ ...paramValues }),
    },
    kv: {
      get: () => Promise.resolve(undefined),
      set: () => Promise.resolve(),
      delete: () => Promise.resolve(),
      list: () => Promise.resolve({}),
    },
    input: undefined,
    output: () => Promise.resolve(),
    mcp: { list_tools: () => Promise.resolve([]), call: () => Promise.resolve({}) },
    dicode: {},
  };
}

Deno.test("rejects when base_url param is missing", async () => {
  if (!RELAY_PRERELEASE_PATH) {
    console.log(PRERELEASE_SKIP_MSG);
    return;
  }
  clearRelayEnv();
  Deno.env.set("STATUS_PASSWORD", "test-pw");
  Deno.env.set("DICODE_DATADIR", "/tmp/dicode-test-datadir");

  const { default: main } = await import("./task.ts");
  const sdk = makeSdk({}); // base_url intentionally unset.

  await assertRejects(
    // deno-lint-ignore no-explicit-any
    () => main(sdk as any),
    Error,
    "base_url param is required",
  );
});

Deno.test("rejects when STATUS_PASSWORD is missing", async () => {
  if (!RELAY_PRERELEASE_PATH) {
    console.log(PRERELEASE_SKIP_MSG);
    return;
  }
  clearRelayEnv();
  Deno.env.set("DICODE_DATADIR", "/tmp/dicode-test-datadir");
  // STATUS_PASSWORD intentionally unset — relay's loadConfig() resolves
  // ${STATUS_PASSWORD} from process.env and must throw on miss.

  const { default: main } = await import("./task.ts");
  const sdk = makeSdk({ base_url: "https://relay.example.com" });

  await assertRejects(
    // deno-lint-ignore no-explicit-any
    () => main(sdk as any),
  );
});

Deno.test(
  "valid base_url + required env → startServer({dryRun:true}) resolves",
  async () => {
    if (!RELAY_PRERELEASE_PATH) {
      console.log(PRERELEASE_SKIP_MSG);
      return;
    }
    clearRelayEnv();
    Deno.env.set("STATUS_PASSWORD", "test-pw");
    Deno.env.set("DICODE_DATADIR", "/tmp/dicode-test-datadir");

    const { startServer } = await import(
      "npm:dicode-relay@^0.2/start"
    ) as {
      startServer: (opts: {
        configPath: string;
        dryRun: boolean;
      }) => Promise<{ close: () => Promise<void> }>;
    };

    Deno.env.set("BASE_URL", "https://relay.example.com");
    const configPath = new URL("./relay.yaml", import.meta.url).pathname;
    const handle = await startServer({ configPath, dryRun: true });
    await handle.close();
  },
);
