// buildin/relay-client — daemon task that maintains the WebSocket tunnel
// to the dicode relay broker. Identity is encrypted-at-rest via
// dicode.crypto + persisted via the configured storage task. Sub-keys
// never cross IPC; this task only sees decrypted plaintext during the
// brief window between fetch and Identity.import().

import {
  Identity,
  RelayClient,
  type StoredIdentity,
} from "npm:dicode-relay@0.2.1/client";

import type { DicodeSdk } from "../../sdk.ts";

const IDENTITY_CTX   = "dicode/relay-identity/v1";
const BROKER_KEY_CTX = "dicode/relay-broker-key/v1";
const PREFIX         = "relay/";
const ID_KEY         = "relay/identity-v1";
const BROKER_KEY_KEY = "relay/broker-key-v1";
const ROOT_DEFAULT   = "relay-store"; // appended under the storage task's data dir

function b64encode(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

function b64decode(s: string): Uint8Array {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

// Initial + max delay for the task-level outer-loop backoff. The npm
// RelayClient.run() loops internally with WSS-level exp backoff, so this
// outer loop only fires when something throws OUTSIDE the WSS lifecycle:
// missing env vars, storage-task failure, decrypt failure during identity
// load, etc. Such errors are usually persistent (config issue) — fast
// retries waste cycles. Cap matches the WSS-level cap for symmetry.
const OUTER_BACKOFF_INITIAL_MS = 5_000;
const OUTER_BACKOFF_MAX_MS = 60_000;

// ── ws abortHandshake fault containment (#460) ──────────────────────────
// When the relay is unreachable, npm ws@8.x can run abortHandshake() from
// an AbortSignal timeout callback after the 'error' handler already nulled
// the underlying ClientRequest, deref'ing req.setHeader. That TypeError is
// thrown OUTSIDE the promise chain RelayClient.run() is awaited on, so the
// outer backoff loop can't catch it — Deno exits 1 and, with restart: never,
// the tunnel stays dead until manual intervention.

// True only for that fault family: a TypeError raised inside ws's
// websocket.js handshake path deref'ing a nulled request. Anything else
// must stay fatal so real bugs aren't masked by the reconnect loop.
export function isAbortedHandshakeFault(err: unknown): boolean {
  if (!(err instanceof TypeError)) return false;
  const stack = err.stack ?? "";
  const inWsHandshake = stack.includes("abortHandshake") ||
    /\bws(@[^/\s]+)?\/lib\/websocket\.js/.test(stack);
  // Both V8 phrasings: "Cannot read properties of null (reading 'x')" and
  // the older "Cannot read property 'x' of null".
  return inWsHandshake && /Cannot read propert(?:y|ies).* of null/.test(err.message);
}

// Set while a RelayClient.run() is in flight; a swallowed fault rejects
// that run's race so the outer backoff loop drives the reconnect.
let onWsFault: ((err: Error) => void) | null = null;

function installWsFaultHandlers(): void {
  const swallow = (ev: Event, err: unknown, kind: string) => {
    if (!isAbortedHandshakeFault(err)) return; // real bug — stay fatal
    ev.preventDefault();
    console.warn(
      `relay-client: swallowed ws handshake fault (${kind}), reconnecting:`,
      (err as Error).message,
    );
    onWsFault?.(err as Error);
  };
  globalThis.addEventListener("error", (ev) => swallow(ev, ev.error, "error"));
  globalThis.addEventListener(
    "unhandledrejection",
    (ev) => swallow(ev, ev.reason, "unhandledrejection"),
  );
}

export default async function main(sdk: DicodeSdk): Promise<void> {
  installWsFaultHandlers();
  let backoff = OUTER_BACKOFF_INITIAL_MS;

  // Outer loop: never exit. task.yaml has restart: never so engine won't
  // re-spawn this task — instead, transient failures (storage hiccup,
  // missing config the operator is about to fix, etc.) are absorbed here
  // with exponential backoff. The only way out is daemon shutdown.
  while (true) {
    try {
      await runOnce(sdk);
      // runOnce only returns on RelayClient.run() returning, which only
      // happens on AbortSignal — i.e. clean daemon shutdown. Exit cleanly.
      return;
    } catch (err) {
      console.error(`relay-client: top-level error, retrying in ${Math.round(backoff / 1000)}s:`, err);
      await new Promise((r) => setTimeout(r, backoff));
      backoff = Math.min(backoff * 2, OUTER_BACKOFF_MAX_MS);
    }
  }
}

// Resolve the broker control-channel URL list. Prefer the multi-instance
// DICODE_RELAY_SERVER_URLS (comma-separated) and fall back to the single
// DICODE_RELAY_SERVER_URL shorthand. A one-element list keeps a single code
// path — the library emits the flat onStatus shape for one URL and adds an
// optional endpoints[] breakdown for many.
export function resolveServerURLs(): string[] {
  const multi = Deno.env.get("DICODE_RELAY_SERVER_URLS") ?? "";
  const urls = multi
    .split(",")
    .map((u) => u.trim())
    .filter((u) => u !== "");
  if (urls.length > 0) return urls;
  const single = (Deno.env.get("DICODE_RELAY_SERVER_URL") ?? "").trim();
  return single !== "" ? [single] : [];
}

async function runOnce(sdk: DicodeSdk): Promise<void> {
  const serverURLs = resolveServerURLs();
  const portStr = Deno.env.get("DICODE_RELAY_LOCAL_PORT") ?? "";
  const localPort = Number(portStr);
  const storageTask = Deno.env.get("DICODE_STORAGE_TASK") ?? "buildin/local-storage";

  if (serverURLs.length === 0) {
    throw new Error("relay-client: no relay server URL set (DICODE_RELAY_SERVER_URLS / DICODE_RELAY_SERVER_URL)");
  }
  if (!Number.isFinite(localPort) || localPort <= 0) {
    throw new Error(`relay-client: DICODE_RELAY_LOCAL_PORT invalid (${portStr})`);
  }

  // ── identity load-or-generate ───────────────────────────────────────
  const identity = await loadOrGenerateIdentity(sdk, storageTask);

  // ── mTLS client cert (wraps the identity sign key) ──────────────────
  // Regenerated per boot; only the SPKI matters to the broker, which
  // derives the daemon uuid from the peer certificate.
  const { certPem, keyPem } = await identity.mintClientCert();

  // CA for verifying the broker's server cert. Absent → the platform trust
  // store (WebPKI), correct for the hosted relay. For a self-hosted broker
  // the daemon exports the operator's ca_file as env-borne PEM. As a fallback
  // — read each connect so it self-heals — trust the in-process relay-server's
  // self-signed cert at its known data-dir path once it has been generated
  // (which happens after the daemon boot that would have exported the PEM).
  let caPem = Deno.env.get("DICODE_RELAY_CA_PEM") ?? "";
  if (caPem === "") {
    const datadir = Deno.env.get("DICODE_DATADIR");
    if (datadir) {
      try {
        caPem = await Deno.readTextFile(`${datadir}/relay/mtls-cert.pem`);
      } catch {
        // Not present (or not readable) → fall through to WebPKI.
      }
    }
  }

  const client = new RelayClient({
    serverURLs,
    localPort,
    identity,
    tls: {
      certPem,
      keyPem,
      ...(caPem !== "" ? { ca: caPem } : {}),
    },
    // The channel is TLS-server-authenticated, so the broker key is trusted
    // and persisted unconditionally (auth-relay reads it out-of-band to
    // verify OAuth delivery envelope signatures).
    onBrokerPubkey: (brokerKeyB64: string) => saveBrokerKey(sdk, storageTask, brokerKeyB64),
    log: console,
    onStatus: (s) => {
      void sdk.kv.set("status", s);
    },
  });

  // Race run() against the process-level fault channel: a swallowed ws
  // handshake fault rejects the race (caught by main's backoff loop) and
  // the abort tears down the abandoned client so reconnects don't stack.
  const abort = new AbortController();
  const fault = new Promise<never>((_, reject) => {
    onWsFault = reject;
  });
  try {
    await Promise.race([client.run(abort.signal), fault]);
  } finally {
    onWsFault = null;
    abort.abort();
  }
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

async function saveBrokerKey(sdk: DicodeSdk, storageTask: string, pubkeyB64: string): Promise<void> {
  const pt = new TextEncoder().encode(pubkeyB64);
  const ct = await sdk.dicode.crypto.encrypt(BROKER_KEY_CTX, pt);
  await putBlob(sdk, storageTask, BROKER_KEY_KEY, ct);
}

function rootFor(): string {
  // The storage task's `root` param defaults to "${DATADIR}/run-inputs".
  // Override with a dedicated relay-store directory so blobs don't mingle.
  // The DATADIR variable is expanded at task-load time by the dicode
  // runtime; we just supply the suffix.
  const datadir = Deno.env.get("DICODE_DATADIR") ?? ".";
  return `${datadir}/${ROOT_DEFAULT}`;
}
