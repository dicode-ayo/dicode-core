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
// just bootstraps the broker signing key and hands off to startServer with
// the pre-rendered path. OAuth secrets no longer flow through this task's
// env — they're scoped to stage 1 of the pipeline.

import { chmodSync, existsSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { generateKeyPairSync } from "node:crypto";
import { startServer } from "npm:dicode-relay@0.2.0/start";
import { generateSelfSignedServerCert } from "npm:dicode-relay@0.2.0/client";
import type { DicodeSdk } from "../../sdk.ts";

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
  // main() runs.
  const configPath = join(dataDir, "relay", "relay.yaml");

  ensureSigningKey();
  await ensureServerCert();

  const handle = await startServer({ configPath });

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
