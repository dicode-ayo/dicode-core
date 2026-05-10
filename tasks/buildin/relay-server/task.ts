// buildin/relay-server — daemon task that runs dicode-relay in-process
// under the daemon's Deno runtime.
//
// OAuth client secrets and the status password are sourced from Doppler via
// the existing buildin/secret-providers/doppler task (declared in task.yaml's
// permissions.env). The only deployment-specific value supplied via task
// params is `base_url`, the public URL the relay is reachable at.
//
// The relay's loadConfig() reads ${VAR} placeholders out of process.env at
// startup, so this task body just surfaces base_url as BASE_URL and hands
// off to the relay's library entry point. No subprocess, no shell-out.

import { chmodSync, existsSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { generateKeyPairSync } from "node:crypto";
import { startServer } from "npm:dicode-relay@^0.1.6/start";
import type { DicodeSdk } from "../../sdk.ts";

// Pre-create the broker signing key under ${DICODE_DATADIR}/relay/ so the
// relay's loadBrokerSigningKey() has a file to read on first run.
//
// Why this lives in the supervisor (us) rather than the relay: dicode-relay
// 0.1.7+ (see dicode-ayo/dicode-relay#73) scopes auto-generation back to the
// legacy <cwd>/broker-signing-key.pem fallback only — when relay.yaml pins
// `broker.signing_key_file: <path>` (as ours does) and the file is missing,
// the relay now throws ENOENT instead of generating one at the configured
// path. Keeping the bootstrap in the supervisor lets the relay stay minimal
// (it just reads the configured path) while we own first-run setup.
//
// Format must match loadBrokerSigningKey(): PKCS8 PEM, P-256 (prime256v1)
// curve. Public key is derived from the private key at relay load time, so
// only the private key is persisted.
export function ensureSigningKey(): void {
  const dataDir = Deno.env.get("DICODE_DATADIR");
  if (!dataDir) {
    throw new Error(
      "DICODE_DATADIR not set; daemon should always provide this",
    );
  }
  const keyPath = join(dataDir, "relay", "broker-signing.key");
  if (existsSync(keyPath)) {
    // Tighten perms for keys that may have been written 0o644 by relay
    // 0.1.6's now-removed auto-generate path before this supervisor took
    // over the bootstrap responsibility. Idempotent on already-0o600 files.
    chmodSync(keyPath, 0o600);
    return;
  }
  mkdirSync(dirname(keyPath), { recursive: true });
  const { privateKey } = generateKeyPairSync("ec", {
    namedCurve: "prime256v1",
    // Both encodings must be supplied together for node:crypto's type
    // overload to resolve to the "string-returning" variant — we ignore
    // the public key (relay derives it from the private at load time).
    publicKeyEncoding: { type: "spki", format: "pem" },
    privateKeyEncoding: { type: "pkcs8", format: "pem" },
  });
  // With privateKeyEncoding set, `privateKey` is a PKCS8 PEM string.
  // flag: "wx" fails loudly if another writer raced us — better than
  // silently clobbering and leaving two daemons disagreeing on the broker
  // pubkey. The `restart: always` policy means a clean re-dispatch will
  // short-circuit on existsSync above.
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

export default async function main({ params }: DicodeSdk): Promise<void> {
  const baseUrl = await params.get("base_url");
  if (!baseUrl) {
    // params.base_url is marked required in task.yaml, so the daemon should
    // refuse to dispatch without it; this guard is defence in depth.
    throw new Error("base_url param is required");
  }

  // Relay's loadConfig() walks YAML strings and replaces ${VAR} from
  // process.env. Deno's node-compat exposes Deno.env as process.env.
  Deno.env.set("BASE_URL", baseUrl);

  // First-run bootstrap: relay 0.1.7+ refuses to auto-generate at a
  // configured signing_key_file path, so create the key before handoff.
  ensureSigningKey();

  const configPath = new URL("./relay.yaml", import.meta.url).pathname;
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
