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
 * Plus the issue #495 default-status-password guard cases at the bottom:
 * isLoopbackBaseURL host classification, guardStatusPassword warn/refuse
 * semantics, and a loadConfig round-trip on the real rendered relay.yaml
 * shape.
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

import { assertEquals, assertRejects, assertThrows } from "jsr:@std/assert@1";
import { parse as parseYaml } from "jsr:@std/yaml@1";
import { createPrivateKey, generateKeyPairSync } from "node:crypto";
import { mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { join } from "node:path";
import { defaultConfig, loadConfig } from "npm:dicode-relay@0.2.0/config";
import type { RelayConfig } from "npm:dicode-relay@0.2.0/config";
import { startServer } from "npm:dicode-relay@0.2.0/start";

import { X509Certificate } from "node:crypto";
import {
  DEFAULT_STATUS_PASSWORD,
  ensureServerCert,
  ensureSigningKey,
  guardStatusPassword,
  isLoopbackBaseURL,
} from "./task.ts";

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

Deno.test("ensureServerCert throws when DICODE_DATADIR is unset", async () => {
  clearRelayEnv();
  Deno.env.delete("DICODE_DATADIR");
  await assertRejects(() => ensureServerCert(), Error, "DICODE_DATADIR");
});

Deno.test(
  "ensureServerCert on fresh datadir writes a parseable P-256 cert at 0o600 key",
  async () => {
    clearRelayEnv();
    const datadir = Deno.makeTempDirSync({ prefix: "dicode-relay-test-" });
    Deno.env.set("DICODE_DATADIR", datadir);
    // Exercise the BASE_URL host path (SAN content is asserted in the relay
    // package's own Node-side mTLS connect tests; node:crypto's
    // X509Certificate.subjectAltName is not readable under Deno node-compat).
    Deno.env.set("BASE_URL", "https://relay.test.example");

    await ensureServerCert();

    const certPath = join(datadir, "relay", "mtls-cert.pem");
    const keyPath = join(datadir, "relay", "mtls-key.pem");
    const cert = new X509Certificate(readFileSync(certPath, "utf8"));
    assertEquals(cert.publicKey.asymmetricKeyType, "ec");
    createPrivateKey(readFileSync(keyPath, "utf8")); // throws if malformed
    assertEquals(statSync(keyPath).mode & 0o777, 0o600);
  },
);

Deno.test(
  "ensureServerCert is idempotent — preserves existing cert/key on subsequent calls",
  async () => {
    clearRelayEnv();
    const datadir = Deno.makeTempDirSync({ prefix: "dicode-relay-test-" });
    Deno.env.set("DICODE_DATADIR", datadir);

    await ensureServerCert();
    const certPath = join(datadir, "relay", "mtls-cert.pem");
    const before = readFileSync(certPath, "utf8");

    await ensureServerCert(); // must not throw, must not regenerate.

    assertEquals(readFileSync(certPath, "utf8"), before);
  },
);

Deno.test(
  "ensureServerCert recovers from partial state (cert present, key missing)",
  async () => {
    clearRelayEnv();
    const datadir = Deno.makeTempDirSync({ prefix: "dicode-relay-test-" });
    Deno.env.set("DICODE_DATADIR", datadir);

    // Simulate a crash between the two writes: an orphaned cert, no key.
    const relayDir = join(datadir, "relay");
    mkdirSync(relayDir, { recursive: true });
    const certPath = join(relayDir, "mtls-cert.pem");
    const keyPath = join(relayDir, "mtls-key.pem");
    writeFileSync(certPath, "stale orphan cert");

    // Must not throw with a misleading EEXIST — it clears the orphan and
    // regenerates both into a consistent, loadable pair.
    await ensureServerCert();

    const cert = new X509Certificate(readFileSync(certPath, "utf8"));
    assertEquals(cert.publicKey.asymmetricKeyType, "ec");
    createPrivateKey(readFileSync(keyPath, "utf8"));
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
  mtls:
    port: 5554
    cert_file: ""
    key_file: ""
status:
  password: "test-pass"
relay:
  timestamp_tolerance_s: 30
  ping_interval_ms: 30000
  pong_timeout_ms: 10000
  request_timeout_ms: 30000
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

// ── issue #495: default status password guard ──────────────────────────────

// configWith builds a RelayConfig fixture from the schema's own defaults
// (per config.d.ts's own guidance: "Use defaultConfig() in tests instead of
// duplicating default values") with just base_url/password overridden.
// dicode-relay@0.2.0's status.password is `string | undefined` after the
// schema's transform — undefined is the "no password configured → /status
// 404s" sentinel ("" is rejected by the schema's min(1) at load time, so a
// parsed config never carries it).
function configWith(
  baseURL: string,
  password: string | undefined,
): RelayConfig {
  const cfg = defaultConfig();
  return {
    ...cfg,
    server: { ...cfg.server, base_url: baseURL },
    status: { ...cfg.status, password },
  };
}

Deno.test("isLoopbackBaseURL recognizes loopback hosts", () => {
  assertEquals(isLoopbackBaseURL("http://localhost:5553"), true);
  assertEquals(isLoopbackBaseURL("http://127.0.0.1:5553"), true);
  assertEquals(isLoopbackBaseURL("http://127.0.0.2:5553"), true); // 127.0.0.0/8, not just .1
  assertEquals(isLoopbackBaseURL("http://127.1.2.3:5553"), true);
  assertEquals(isLoopbackBaseURL("http://[::1]:5553"), true);
  assertEquals(isLoopbackBaseURL("http://dev.localhost:5553"), true);
});

Deno.test("isLoopbackBaseURL rejects public hosts and malformed/schemeless URLs", () => {
  assertEquals(isLoopbackBaseURL("https://relay.example.com"), false);
  assertEquals(isLoopbackBaseURL("https://relay.dicode.app"), false);
  assertEquals(isLoopbackBaseURL("http://128.0.0.1:5553"), false); // not 127.x
  assertEquals(isLoopbackBaseURL("::::not-a-url::::"), false);
  // A scheme with no "//" authority (e.g. an operator forgetting the
  // "http://" prefix) parses to an empty hostname, not a loopback one.
  assertEquals(isLoopbackBaseURL("relay.example.com:5553"), false);
});

Deno.test(
  "guardStatusPassword throws when the default password guards a non-loopback base_url",
  () => {
    const config = configWith(
      "https://relay.example.com",
      DEFAULT_STATUS_PASSWORD,
    );
    assertThrows(
      () => guardStatusPassword(config),
      Error,
      "refusing to start",
    );
  },
);

Deno.test(
  "guardStatusPassword does not throw for the default password on a loopback base_url",
  () => {
    const config = configWith("http://127.0.0.1:5553", DEFAULT_STATUS_PASSWORD);
    guardStatusPassword(config); // must not throw
  },
);

Deno.test(
  "guardStatusPassword does not throw when a real password is configured",
  () => {
    const config = configWith("https://relay.example.com", "a-real-secret");
    guardStatusPassword(config); // must not throw
  },
);

Deno.test(
  "guardStatusPassword does not throw when no password is configured (status disabled)",
  () => {
    // 0.2.0 collapses an absent/null status.password to undefined — the
    // relay disables /status entirely (404s), which is safe.
    const config = configWith("https://relay.example.com", undefined);
    guardStatusPassword(config); // must not throw
  },
);

Deno.test(
  "end-to-end: loadConfig on the real rendered relay.yaml shape feeds guardStatusPassword correctly",
  () => {
    // Mirrors what the buildin/template + buildin/write-local pipeline
    // stages actually produce (see ../relay-server/relay.yaml) after
    // substituting BASE_URL/STATUS_PASSWORD — guards against the guard's
    // understanding of the config drifting from what dicode-relay itself
    // parses (rather than testing against a hand-built RelayConfig object).
    const datadir = Deno.makeTempDirSync({ prefix: "dicode-relay-test-" });
    mkdirSync(join(datadir, "relay"), { recursive: true });
    const configPath = join(datadir, "relay", "relay.yaml");

    writeFileSync(
      configPath,
      `server:
  port: 5553
  base_url: https://relay.example.com
  tls:
    cert_file: ""
    key_file: ""
  mtls:
    port: 5554
    cert_file: ${datadir}/relay/mtls-cert.pem
    key_file: ${datadir}/relay/mtls-key.pem
status:
  password: ${DEFAULT_STATUS_PASSWORD}
relay:
  timestamp_tolerance_s: 30
  ping_interval_ms: 30000
  pong_timeout_ms: 10000
  request_timeout_ms: 30000
broker:
  session_ttl_ms: 300000
  signing_key_file: ${datadir}/relay/broker-signing.key
`,
      { mode: 0o600 },
    );

    const config = loadConfig(undefined, configPath);
    assertThrows(
      () => guardStatusPassword(config),
      Error,
      "refusing to start",
    );
  },
);

Deno.test(
  "loadConfig throws a clear error when relay.yaml hasn't been rendered yet",
  () => {
    // Covers the "Standalone-runnable" path documented in this task's
    // task.yaml: an operator can run this body directly, before the
    // buildin/relay-server pipeline's render stages have ever produced
    // relay.yaml. loadConfig (called from main(), before ensureSigningKey)
    // must fail with its own clear message rather than a bare ENOENT.
    const datadir = Deno.makeTempDirSync({ prefix: "dicode-relay-test-" });
    const missingConfigPath = join(datadir, "relay", "relay.yaml");
    assertThrows(
      () => loadConfig(undefined, missingConfigPath),
      Error,
      "Config file not found",
    );
  },
);

Deno.test(
  "DEFAULT_STATUS_PASSWORD matches the documented dev fallback in relay-server/task.yaml",
  () => {
    // Locks DEFAULT_STATUS_PASSWORD to the actual `default:` value the
    // pipeline's render stage uses, so the two can't silently drift apart
    // (they're independently declared — see the comment on
    // DEFAULT_STATUS_PASSWORD in task.ts).
    const taskYamlPath = fileURLToPath(
      new URL("../relay-server/task.yaml", import.meta.url),
    );
    // deno-lint-ignore no-explicit-any
    const parsed = parseYaml(readFileSync(taskYamlPath, "utf8")) as any;
    const envEntries = parsed.stages[0].overrides.env as Array<
      { name: string; default?: string }
    >;
    const statusPasswordEntry = envEntries.find((e) =>
      e.name === "STATUS_PASSWORD"
    );
    assertEquals(statusPasswordEntry?.default, DEFAULT_STATUS_PASSWORD);
  },
);
