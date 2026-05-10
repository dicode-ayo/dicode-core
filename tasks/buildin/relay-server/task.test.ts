/**
 * task.test.ts — unit tests for buildin/relay-server.
 *
 * Exercises three cases the spec called out:
 *   1. base_url param missing → task body throws before touching the relay.
 *   2. broker signing-key file pinned in relay.yaml but not on disk →
 *      startServer rejects (the published relay's loadBrokerSigningKey()
 *      does an unconditional readFileSync when the YAML pins a path, even
 *      under dryRun, so this is the real "required artefact missing"
 *      failure mode surfaced by 0.1.6).
 *   3. valid base_url + ${DICODE_DATADIR}/relay/broker-signing.key staged
 *      on disk + STATUS_PASSWORD set → startServer({dryRun:true}) resolves.
 *
 * Cases 1 and 2 exercise the task body (./task.ts) via a hand-rolled SDK
 * mock — the harness in tasks/sdk-test.ts would dynamically re-import
 * task.ts and lose env determinism between cases. Case 3 calls startServer
 * directly so it stays decoupled from the supervisor's signal-handling
 * scaffolding, which would otherwise leak Deno.addSignalListener handles
 * into the test runner.
 *
 * Note (deviation from spec): the spec called for a "STATUS_PASSWORD
 * missing → throws" case. The published 0.1.6 relay's loadConfig() does
 * not throw on unresolved ${VAR} — it logs a warning and collapses to "".
 * The actual hard-failure surface for missing required artefacts is the
 * broker signing-key file, hence case 2 above.
 */

import { assertRejects } from "jsr:@std/assert@1";
import { generateKeyPairSync } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { startServer } from "npm:dicode-relay@^0.1.6/start";

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
  "BROKER_SIGNING_KEY",
  "BROKER_SIGNING_KEY_FILE",
];

function clearRelayEnv(): void {
  for (const k of RELAY_ENV_VARS) Deno.env.delete(k);
}

/**
 * Create a fresh DICODE_DATADIR under a unique temp dir, seed it with a
 * valid ECDSA P-256 broker signing key at ${DICODE_DATADIR}/relay/
 * broker-signing.key, and export DICODE_DATADIR so the YAML's
 * ${DICODE_DATADIR}/relay/broker-signing.key reference resolves to it.
 */
function stageDatadirWithSigningKey(): string {
  const datadir = Deno.makeTempDirSync({ prefix: "dicode-relay-test-" });
  mkdirSync(join(datadir, "relay"), { recursive: true });
  const { privateKey } = generateKeyPairSync("ec", {
    namedCurve: "prime256v1",
    publicKeyEncoding: { type: "spki", format: "pem" },
    privateKeyEncoding: { type: "pkcs8", format: "pem" },
  });
  writeFileSync(join(datadir, "relay", "broker-signing.key"), privateKey, {
    mode: 0o600,
  });
  Deno.env.set("DICODE_DATADIR", datadir);
  return datadir;
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
  clearRelayEnv();
  Deno.env.set("STATUS_PASSWORD", "test-pw");
  stageDatadirWithSigningKey();

  const { default: main } = await import("./task.ts");
  const sdk = makeSdk({}); // base_url intentionally unset.

  await assertRejects(
    // deno-lint-ignore no-explicit-any
    () => main(sdk as any),
    Error,
    "base_url param is required",
  );
});

Deno.test(
  "rejects when broker signing-key file pinned in relay.yaml is missing",
  async () => {
    clearRelayEnv();
    Deno.env.set("STATUS_PASSWORD", "test-pw");
    // DICODE_DATADIR points at a tempdir but we intentionally do NOT
    // seed broker-signing.key — startServer's loadBrokerSigningKey does
    // a readFileSync against the YAML-pinned path and surfaces ENOENT.
    const datadir = Deno.makeTempDirSync({ prefix: "dicode-relay-test-" });
    Deno.env.set("DICODE_DATADIR", datadir);

    const { default: main } = await import("./task.ts");
    const sdk = makeSdk({ base_url: "https://relay.example.com" });

    await assertRejects(
      // deno-lint-ignore no-explicit-any
      () => main(sdk as any),
    );
  },
);

Deno.test(
  "valid base_url + staged datadir → startServer({dryRun:true}) resolves",
  async () => {
    clearRelayEnv();
    Deno.env.set("STATUS_PASSWORD", "test-pw");
    stageDatadirWithSigningKey();
    Deno.env.set("BASE_URL", "https://relay.example.com");

    const configPath = new URL("./relay.yaml", import.meta.url).pathname;
    const handle = await startServer({ configPath, dryRun: true });
    await handle.close();
  },
);
