// buildin/relay-client — daemon task that maintains the WebSocket tunnel
// to the dicode relay broker. Identity is encrypted-at-rest via
// dicode.crypto + persisted via the configured storage task. Sub-keys
// never cross IPC; this task only sees decrypted plaintext during the
// brief window between fetch and Identity.import().

import {
  Identity,
  RelayClient,
  type StoredIdentity,
  type TofuResult,
} from "npm:dicode-relay@^0.1.4/client";

const IDENTITY_CTX   = "dicode/relay-identity/v1";
const BROKER_PIN_CTX = "dicode/relay-broker-pin/v1";
const PREFIX         = "relay/";
const ID_KEY         = "relay/identity-v1";
const BROKER_PIN_KEY = "relay/broker-pin-v1";
const ROOT_DEFAULT   = "relay-store"; // appended under the storage task's data dir

function b64encode(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

function b64decode(s: string): Uint8Array {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

export default async function main(sdk: DicodeSdk): Promise<void> {
  const url = Deno.env.get("DICODE_RELAY_SERVER_URL");
  const portStr = Deno.env.get("DICODE_RELAY_LOCAL_PORT") ?? "";
  const localPort = Number(portStr);
  const storageTask = Deno.env.get("DICODE_STORAGE_TASK") ?? "buildin/local-storage";

  if (!url) {
    throw new Error("relay-client: DICODE_RELAY_SERVER_URL not set");
  }
  if (!Number.isFinite(localPort) || localPort <= 0) {
    throw new Error(`relay-client: DICODE_RELAY_LOCAL_PORT invalid (${portStr})`);
  }

  // ── identity load-or-generate ───────────────────────────────────────
  const identity = await loadOrGenerateIdentity(sdk, storageTask);

  // ── TOFU pin via storage task ───────────────────────────────────────
  const tofuCheckAndPin = async (brokerPubkeyB64: string): Promise<TofuResult> => {
    const stored = await fetchPin(sdk, storageTask);
    if (stored === null) {
      await savePin(sdk, storageTask, brokerPubkeyB64);
      return "new";
    }
    return stored === brokerPubkeyB64 ? "match" : "mismatch";
  };

  const client = new RelayClient({
    serverURL:      url,
    localPort,
    identity,
    tofuCheckAndPin,
    log: console,
    onStatus: (s) => {
      void sdk.kv.set("status", s);
    },
  });

  await client.run();
}

async function loadOrGenerateIdentity(
  sdk: DicodeSdk,
  storageTask: string,
): Promise<Identity> {
  // 1. Try fetch + decrypt.
  const enc = await fetchBlob(sdk, storageTask, ID_KEY);
  if (enc !== null) {
    try {
      const pt = await sdk.dicode.crypto.decrypt(IDENTITY_CTX, enc);
      const stored = JSON.parse(new TextDecoder().decode(pt)) as StoredIdentity;
      return await Identity.import(stored);
    } catch (err) {
      console.error("relay-client: failed to decrypt stored identity, regenerating:", err);
      // Fall through to regenerate. This path occurs if the master key
      // changed (passphrase rotation) — in which case the old blob is
      // unrecoverable and a fresh identity is the only recovery.
    }
  }

  // 2. Generate fresh + encrypt + store.
  const id = await Identity.generate();
  const stored = await id.export();
  const pt = new TextEncoder().encode(JSON.stringify(stored));
  const ct = await sdk.dicode.crypto.encrypt(IDENTITY_CTX, pt);
  await putBlob(sdk, storageTask, ID_KEY, ct);
  console.log("relay-client: generated fresh identity, uuid =", id.uuid);
  return id;
}

// dicode.run_task returns a RunResult envelope: { runID, status, returnValue }.
// Unwrap it to get the storage task's actual return value.
function unwrapRunResult(raw: unknown): { ok: boolean; value?: string; error?: string } {
  const envelope = raw as { returnValue?: unknown };
  const rv = envelope?.returnValue ?? raw;
  return rv as { ok: boolean; value?: string; error?: string };
}

async function fetchBlob(
  sdk: DicodeSdk,
  storageTask: string,
  key: string,
): Promise<Uint8Array | null> {
  const raw = await sdk.dicode.run_task(storageTask, {
    op: "get",
    key,
    prefix: PREFIX,
    root: rootFor(),
  });
  const res = unwrapRunResult(raw);
  if (!res.ok) {
    throw new Error(`storage get failed: ${res.error ?? "unknown"}`);
  }
  if (!res.value) return null;
  return b64decode(res.value);
}

async function putBlob(
  sdk: DicodeSdk,
  storageTask: string,
  key: string,
  bytes: Uint8Array,
): Promise<void> {
  const raw = await sdk.dicode.run_task(storageTask, {
    op: "put",
    key,
    value: b64encode(bytes),
    prefix: PREFIX,
    root: rootFor(),
  });
  const res = unwrapRunResult(raw);
  if (!res.ok) throw new Error(`storage put failed: ${res.error ?? "unknown"}`);
}

async function fetchPin(sdk: DicodeSdk, storageTask: string): Promise<string | null> {
  const enc = await fetchBlob(sdk, storageTask, BROKER_PIN_KEY);
  if (!enc) return null;
  const pt = await sdk.dicode.crypto.decrypt(BROKER_PIN_CTX, enc);
  return new TextDecoder().decode(pt);
}

async function savePin(sdk: DicodeSdk, storageTask: string, pubkeyB64: string): Promise<void> {
  const pt = new TextEncoder().encode(pubkeyB64);
  const ct = await sdk.dicode.crypto.encrypt(BROKER_PIN_CTX, pt);
  await putBlob(sdk, storageTask, BROKER_PIN_KEY, ct);
}

function rootFor(): string {
  // The storage task's `root` param defaults to "${DATADIR}/run-inputs".
  // Override with a dedicated relay-store directory so blobs don't mingle.
  // The DATADIR variable is expanded at task-load time by the dicode
  // runtime; we just supply the suffix.
  const datadir = Deno.env.get("DICODE_DATADIR") ?? ".";
  return `${datadir}/${ROOT_DEFAULT}`;
}
