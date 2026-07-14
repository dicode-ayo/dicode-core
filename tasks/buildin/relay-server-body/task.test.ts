/**
 * task.test.ts — unit tests for buildin/relay-server-body.
 *
 * Exercises four cases:
 *   1. DICODE_DATADIR unset → ensureSigningKey throws with a clear error.
 *   2. ensureSigningKey on a fresh datadir creates a readable PKCS8 PEM at
 *      ${DICODE_DATADIR}/relay/broker-signing.key with mode 0o600.
 *   3. ensureSigningKey is idempotent — when the file already exists it
 *      neither regenerates nor throws.
 *   4. valid ${DICODE_DATADIR}/relay/{broker-signing.key,relay.yaml} staged
 *      on disk → startServer({dryRun:true}) resolves.
 *
 * Cases 1-3 call the exported ensureSigningKey directly. Case 4 calls
 * startServer directly (not via the task body's main()) so it stays
 * decoupled from the supervisor's signal-handling scaffolding, which would
 * otherwise leak Deno.addSignalListener handles into the test runner.
 *
 * Note on the dropped "base_url param missing" case: relay.yaml rendering
 * lives in the buildin/relay-server PipelineTask's render stages
 * (buildin/template → buildin/write-local), so this body no longer reads a
 * base_url param — BASE_URL is set as an env entry on the template stage's
 * override in ../relay-server/task.yaml. There is no longer a "missing
 * base_url" failure mode reachable from main().
 *
 * Note (carried from origin/main): the spec called for a "STATUS_PASSWORD
 * missing → throws" case. The published 0.1.6 relay's loadConfig() does
 * not throw on unresolved ${VAR} — it logs a warning and collapses to "".
 * Under 0.1.7+ the relay also throws ENOENT if the signing key file is
 * missing — but the supervisor now pre-creates it (see ensureSigningKey),
 * so the "missing required artefact" failure mode is no longer reachable
 * from the task body's happy path.
 */

import { assertEquals, assertThrows } from "jsr:@std/assert@1";
import { createPrivateKey, generateKeyPairSync } from "node:crypto";
import { mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { startServer } from "npm:dicode-relay@0.1.9/start";

import { ensureSigningKey } from "./task.ts";

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
 * broker-signing.key, and export DICODE_DATADIR so subsequent calls
 * resolve to it.
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

Deno.test("ensureSigningKey throws when DICODE_DATADIR is unset", () => {
  clearRelayEnv();
  Deno.env.delete("DICODE_DATADIR");
  assertThrows(() => ensureSigningKey(), Error, "DICODE_DATADIR");
});

Deno.test(
  "ensureSigningKey on fresh datadir creates a loadable PKCS8 PEM at 0o600",
  () => {
    clearRelayEnv();
    const datadir = Deno.makeTempDirSync({ prefix: "dicode-relay-test-" });
    Deno.env.set("DICODE_DATADIR", datadir);

    ensureSigningKey();

    const keyPath = join(datadir, "relay", "broker-signing.key");
    const pem = readFileSync(keyPath, "utf8");
    // Sanity: PEM is PKCS8 (header reads "PRIVATE KEY", not "EC PRIVATE KEY"
    // which would be SEC1) and parses as a P-256 key via node:crypto, which
    // is what relay's loadBrokerSigningKey() does internally.
    if (!pem.includes("BEGIN PRIVATE KEY")) {
      throw new Error(
        `expected PKCS8 PEM header at ${keyPath}, got:\n${pem.slice(0, 80)}`,
      );
    }
    createPrivateKey(pem); // throws if malformed

    // 0o600 = owner rw only. Linux honours this; on macOS the umask may
    // widen the result but the mode bits we asked for should still be set.
    const mode = statSync(keyPath).mode & 0o777;
    assertEquals(mode, 0o600);
  },
);

Deno.test(
  "ensureSigningKey is idempotent — preserves existing key on subsequent calls",
  () => {
    clearRelayEnv();
    const datadir = stageDatadirWithSigningKey();
    const keyPath = join(datadir, "relay", "broker-signing.key");
    const before = readFileSync(keyPath, "utf8");

    ensureSigningKey(); // must not throw, must not regenerate.

    const after = readFileSync(keyPath, "utf8");
    assertEquals(after, before);
  },
);

Deno.test(
  "staged datadir + pre-rendered relay.yaml → startServer({dryRun:true}) resolves",
  async () => {
    clearRelayEnv();
    const datadir = stageDatadirWithSigningKey();

    // PR3: the pipeline stages (buildin/template → buildin/write-local)
    // render this YAML before the daemon's main() runs. In production it
    // gets the ${VAR} placeholders substituted by the renderer's per-edge
    // env override; here we stage a literal-value YAML so loadConfig
    // doesn't depend on STATUS_PASSWORD / BASE_URL being set in the test's
    // process env.
    const relayYaml = `
server:
  port: 5553
  base_url: "https://test.example.com"
  tls:
    cert_file: ""
    key_file: ""
status:
  password: "test-pass"
relay:
  timestamp_tolerance_s: 30
  ping_interval_ms: 30000
  pong_timeout_ms: 10000
  request_timeout_ms: 30000
  nonce_ttl_ms: 60000
broker:
  session_ttl_ms: 300000
  signing_key_file: ${datadir}/relay/broker-signing.key
  providers: {}
`;
    const configPath = join(datadir, "relay", "relay.yaml");
    writeFileSync(configPath, relayYaml, { mode: 0o600 });

    const handle = await startServer({ configPath, dryRun: true });
    await handle.close();
  },
);
