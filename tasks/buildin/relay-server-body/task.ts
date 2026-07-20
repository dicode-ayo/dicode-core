// buildin/relay-server-body — daemon task that runs dicode-relay in-process
// under the daemon's Deno runtime.
//
// This is the terminal stage of the buildin/relay-server PipelineTask.
// Config rendering is owned by the pipeline's earlier stages:
//
//   stage 1: buildin/template renders relay.yaml from Doppler-fed env
//   stage 2: buildin/write-local persists it to ${DATADIR}/relay/relay.yaml
//
// (See ../relay-server/task.yaml for the pipeline wiring.) This task body
// bootstraps the broker signing key, loads the pre-rendered config (so the
// status-password guard below can inspect it), and hands the parsed config
// off to startServer. OAuth secrets no longer flow through this task's
// env — they're scoped to stage 1 of the pipeline.

import { chmodSync, existsSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { generateKeyPairSync } from "node:crypto";
import { loadConfig } from "npm:dicode-relay@0.2.1/config";
import type { RelayConfig } from "npm:dicode-relay@0.2.1/config";
import { startServer } from "npm:dicode-relay@0.2.1/start";
import { generateSelfSignedServerCert } from "npm:dicode-relay@0.2.1/client";
import type { DicodeSdk } from "../../sdk.ts";

// The documented dev fallback from ../relay-server/task.yaml's STATUS_PASSWORD
// entry (`default: "dicode-relay-dev"`). Convenient for local dev, but a
// publicly-known credential must never protect a non-loopback status
// endpoint — see issue #495.
export const DEFAULT_STATUS_PASSWORD = "dicode-relay-dev";

// isLoopbackBaseURL reports whether base_url only ever binds/advertises a
// local address. A malformed URL is treated as non-loopback — fail toward
// the warning, not away from it.
export function isLoopbackBaseURL(baseURL: string): boolean {
  let host: string;
  try {
    host = new URL(baseURL).hostname;
  } catch {
    return false;
  }
  // WHATWG URL.hostname renders an IPv6 literal with its brackets intact
  // (e.g. "http://[::1]:5553" → "[::1]"), so both forms are checked.
  if (host === "localhost" || host === "::1" || host === "[::1]") {
    return true;
  }
  if (host.endsWith(".localhost")) return true;
  // The entire 127.0.0.0/8 block is loopback, not just 127.0.0.1.
  const ipv4 = host.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  return ipv4 !== null && ipv4.slice(1).every((o) => Number(o) <= 255) &&
    ipv4[1] === "127";
}

// guardStatusPassword is called once at startup on the already-validated
// RelayConfig (loaded via the relay package's own loadConfig — see main()
// below), right before startServer. It never blocks the loopback/dev case
// (a loud warning is enough there) but refuses to start when the same
// well-known default is about to guard a status endpoint reachable from
// outside the loopback interface (issue #495).
//
// Note: under dicode-relay 0.2.0, status.password is `string | undefined` —
// undefined means the status endpoint is disabled entirely (/status 404s),
// which is safe and returns from the guard immediately.
export function guardStatusPassword(config: RelayConfig): void {
  if (config.status.password !== DEFAULT_STATUS_PASSWORD) return;

  console.warn(
    `[relay-server] WARNING: the status endpoint is using the default dev password ` +
      `("${DEFAULT_STATUS_PASSWORD}"). Set a real one with ` +
      `'dicode secrets set RELAY_STATUS_PASSWORD <password>' before exposing this relay.`,
  );

  if (!isLoopbackBaseURL(config.server.base_url)) {
    throw new Error(
      `[relay-server] refusing to start: base_url (${config.server.base_url}) is not a ` +
        `loopback address, but the status endpoint is still guarded by the public default ` +
        `password ("${DEFAULT_STATUS_PASSWORD}"). Set RELAY_STATUS_PASSWORD via ` +
        `'dicode secrets set RELAY_STATUS_PASSWORD <password>' and re-render relay.yaml ` +
        `before exposing this relay beyond localhost.`,
    );
  }
}

// Pre-create the broker signing key under ${DICODE_DATADIR}/relay/ so
// the relay's loadBrokerSigningKey() has a file to read on first run.
// (Verbatim from the previous version — relay 0.1.7+ no longer
// auto-generates at configured signing_key_file paths, so the
// supervisor owns first-run bootstrap.)
export function ensureSigningKey(): void {
  const dataDir = Deno.env.get("DICODE_DATADIR");
  if (!dataDir) {
    throw new Error(
      "DICODE_DATADIR not set; daemon should always provide this",
    );
  }
  const keyPath = join(dataDir, "relay", "broker-signing.key");
  if (existsSync(keyPath)) {
    chmodSync(keyPath, 0o600);
    return;
  }
  mkdirSync(dirname(keyPath), { recursive: true });
  const { privateKey } = generateKeyPairSync("ec", {
    namedCurve: "prime256v1",
    publicKeyEncoding: { type: "spki", format: "pem" },
    privateKeyEncoding: { type: "pkcs8", format: "pem" },
  });
  try {
    writeFileSync(keyPath, privateKey, { flag: "wx", mode: 0o600 });
  } catch (err) {
    throw new Error(
      `[relay-server] failed to write broker signing key at ${keyPath}: ` +
        `${err instanceof Error ? err.message : String(err)}. ` +
        `Check that DICODE_DATADIR (${dataDir}) is writable and has space.`,
    );
  }
  console.warn(
    `[relay-server] generated new broker signing key at ${keyPath} (first-run bootstrap)`,
  );
}

// Pre-create the mTLS server cert/key under ${DICODE_DATADIR}/relay/ so the
// relay's mTLS listener has a certificate to present. Self-signed, CA:FALSE
// (rustls-based Deno daemons reject CA-flagged end-entity certs). Daemons
// trust it by pointing relay.ca_file at the cert. SANs cover localhost plus
// the BASE_URL host so a same-box or hostname-addressed daemon verifies it.
export async function ensureServerCert(): Promise<void> {
  const dataDir = Deno.env.get("DICODE_DATADIR");
  if (!dataDir) {
    throw new Error("DICODE_DATADIR not set; daemon should always provide this");
  }
  const certPath = join(dataDir, "relay", "mtls-cert.pem");
  const keyPath = join(dataDir, "relay", "mtls-key.pem");
  const certExists = existsSync(certPath);
  const keyExists = existsSync(keyPath);
  if (certExists && keyExists) {
    chmodSync(keyPath, 0o600);
    return;
  }
  // Partial state — a crash between the two writes below leaves one file
  // orphaned. Clear it so the exclusive-create writes don't EEXIST-fail on
  // every subsequent boot; regenerate the pair together.
  if (certExists) rmSync(certPath);
  if (keyExists) rmSync(keyPath);

  // Loopback SANs are listed explicitly so a same-box daemon dialing
  // localhost verifies regardless of the helper's own default SANs.
  const hosts: string[] = ["127.0.0.1", "localhost"];
  const baseURL = Deno.env.get("BASE_URL");
  if (baseURL) {
    try {
      const host = new URL(baseURL).hostname;
      if (host !== "") hosts.push(host);
    } catch {
      // BASE_URL unparsable — the loopback SANs above still apply.
    }
  }

  const { certPem, keyPem } = await generateSelfSignedServerCert({ hosts });
  mkdirSync(dirname(certPath), { recursive: true });
  try {
    writeFileSync(certPath, certPem, { flag: "wx", mode: 0o644 });
    writeFileSync(keyPath, keyPem, { flag: "wx", mode: 0o600 });
  } catch (err) {
    throw new Error(
      `[relay-server] failed to write mTLS server cert at ${certPath}: ` +
        `${err instanceof Error ? err.message : String(err)}. ` +
        `Check that DICODE_DATADIR (${dataDir}) is writable and has space.`,
    );
  }
  console.warn(
    `[relay-server] generated self-signed mTLS server cert at ${certPath} ` +
      `(first-run bootstrap) — point daemons' relay.ca_file at it`,
  );
}

export default async function main(_: DicodeSdk): Promise<void> {
  const dataDir = Deno.env.get("DICODE_DATADIR");
  if (!dataDir) {
    throw new Error(
      "DICODE_DATADIR not set; daemon should always provide this",
    );
  }

  // The pipeline's render stages (buildin/template → buildin/write-local)
  // have already rendered relay.yaml and written it to disk before this
  // main() runs. loadConfig throws its own "Config file not found" error
  // when standalone-run before that render has ever happened (see the
  // "Standalone-runnable" note in this task's task.yaml). Loading here —
  // rather than letting startServer load from configPath — lets the
  // status-password guard inspect the parsed config before anything binds.
  const configPath = join(dataDir, "relay", "relay.yaml");
  const config = loadConfig(undefined, configPath);

  guardStatusPassword(config);

  ensureSigningKey();
  await ensureServerCert();

  const handle = await startServer({ config });

  // First buildin to use Deno.addSignalListener directly: the relay's
  // startServer returns a long-lived http.Server rather than an abortable
  // promise, so we have to bridge OS signals to handle.close() manually.
  // Future buildins that expose AbortSignal should prefer that pattern.
  //
  // No explicit Deno.exit(0): handle.close() tears down the http.Server,
  // which fires the `close` event below, which resolves main(). Exiting
  // explicitly bypasses the daemon engine's "task returned" accounting
  // (compare relay-client/task.ts) and short-circuits any in-flight
  // microtasks the relay's close() queued.
  const shutdown = async () => {
    await handle.close();
  };
  Deno.addSignalListener("SIGTERM", shutdown);
  Deno.addSignalListener("SIGINT", shutdown);

  // Keep the daemon task alive while the relay is running. RelayServer's
  // underlying http.Server emits `close` when listen() is undone (clean
  // shutdown or crash); resolving here lets the task exit cleanly so the
  // engine can apply its restart policy.
  await new Promise<void>((resolve) => {
    handle.httpServer.on("close", () => resolve());
  });
}
