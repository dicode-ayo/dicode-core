// buildin/auth-relay — OAuth token delivery sink.
//
// The relay broker POSTs an OAuthTokenDeliveryPayload JSON envelope to
// /hooks/oauth-complete via the WSS tunnel. This task verifies the broker
// signature and ECIES-decrypts the envelope using the daemon's relay
// identity (loaded encrypted from the configured storage task), then
// persists the resulting token fields to the secrets store via
// dicode.secrets_set.
//
// Plaintext credentials live in this task's memory between decrypt and
// secrets_set. The task is locked down to make exfiltration impossible:
//   - silent: true (stdout/stderr → io.Discard; console.log writes nothing)
//   - permissions.net: [] (no fetch / WebSocket / outbound)
//   - permissions.fs: [] (no Deno.writeFile)
//   - permissions.env: minimum subset (read-only, harmless config vars)
// The only output channels are dicode.secrets_set (the goal) and
// dicode.run_task to the storage task (which only ever sees ciphertexts).

import {
  Identity,
  decryptTokenEnvelope,
  type StoredIdentity,
} from "npm:dicode-relay@^0.1.4/client";

const IDENTITY_CTX   = "dicode/relay-identity/v1";
const BROKER_PIN_CTX = "dicode/relay-broker-pin/v1";
const PENDING_CTX    = "dicode/oauth-pending/v1";
const PREFIX         = "relay/";
const ID_KEY         = "relay/identity/v1";
const BROKER_PIN_KEY = "relay/broker-pin/v1";

function b64decode(s: string): Uint8Array {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

interface OAuthEnvelope {
  type: "oauth_token_delivery";
  session_id: string;
  ephemeral_pubkey: string;
  ciphertext: string;
  nonce: string;
  broker_sig?: string;
}

export default async function main({ input, dicode }: DicodeSdk) {
  const envelope = input as OAuthEnvelope;
  if (!envelope || envelope.type !== "oauth_token_delivery") {
    throw new Error("invalid OAuth token delivery envelope");
  }

  const datadir     = Deno.env.get("DICODE_DATADIR") ?? ".";
  const root        = `${datadir}/relay-store`;
  const storageTask = Deno.env.get("DICODE_STORAGE_TASK") ?? "buildin/local-storage";

  // 1. Load identity.
  const identity = await loadIdentity(dicode, storageTask, root);

  // 2. Load broker pubkey (TOFU-pinned by relay-client task on first connect).
  const brokerPubkey = await loadBrokerPin(dicode, storageTask, root);
  if (!brokerPubkey) {
    throw new Error("broker pubkey not pinned — has the relay-client connected yet?");
  }

  // 3. Verify broker_sig + ECIES-decrypt. Throws if either fails.
  const tokens = await decryptTokenEnvelope(envelope, identity, brokerPubkey);

  // 4. Resolve provider from the pending-session record written by auth-start.
  const provider = await resolveProvider(dicode, storageTask, root, envelope.session_id);

  // 5. Write each token field to secrets. Provider-specific naming follows
  //    the convention from the legacy dicode.oauth.store_token primitive:
  //    <PROVIDER>_<FIELD_UPPER>.
  const written: string[] = [];
  for (const [field, value] of Object.entries(tokens)) {
    if (typeof value !== "string" || !value) continue;
    const secretName = `${provider.toUpperCase()}_${field.toUpperCase()}`;
    await dicode.secrets_set(secretName, value);
    written.push(secretName);
  }

  // Delete pending record to prevent replay. Best-effort — if delete fails
  // the record is harmless (just leaks until cleanup), but the security
  // benefit comes from making it unavailable for replays.
  try {
    await dicode.run_task(storageTask, {
      op: "delete",
      key: `oauth-pending/${envelope.session_id}`,
      prefix: "oauth-pending/",
      root,
    });
  } catch {
    // Ignore — log nothing because the task is silent: true.
  }

  return {
    ok: true,
    provider,
    secrets_written: written,
  };
}

async function loadIdentity(
  dicode: DicodeSdk["dicode"],
  storageTask: string,
  root: string,
): Promise<Identity> {
  const res = (await dicode.run_task(storageTask, {
    op: "get", key: ID_KEY, prefix: PREFIX, root,
  })) as { ok: boolean; value?: string; error?: string };
  if (!res.ok || !res.value) {
    throw new Error("relay identity not found — has the relay-client task started?");
  }
  const ct = b64decode(res.value);
  const pt = await dicode.crypto.decrypt(IDENTITY_CTX, ct);
  const stored = JSON.parse(new TextDecoder().decode(pt)) as StoredIdentity;
  return await Identity.import(stored);
}

async function loadBrokerPin(
  dicode: DicodeSdk["dicode"],
  storageTask: string,
  root: string,
): Promise<string | null> {
  const res = (await dicode.run_task(storageTask, {
    op: "get", key: BROKER_PIN_KEY, prefix: PREFIX, root,
  })) as { ok: boolean; value?: string };
  if (!res.ok || !res.value) return null;
  const ct = b64decode(res.value);
  const pt = await dicode.crypto.decrypt(BROKER_PIN_CTX, ct);
  return new TextDecoder().decode(pt);
}

async function resolveProvider(
  dicode: DicodeSdk["dicode"],
  storageTask: string,
  root: string,
  sessionId: string,
): Promise<string> {
  const res = (await dicode.run_task(storageTask, {
    op:     "get",
    key:    `oauth-pending/${sessionId}`,
    prefix: "oauth-pending/",
    root,
  })) as { ok: boolean; value?: string };
  if (!res.ok || !res.value) {
    throw new Error("oauth-pending record not found — session expired or never started");
  }
  const ct = b64decode(res.value);
  const pt = await dicode.crypto.decrypt(PENDING_CTX, ct);
  const rec = JSON.parse(new TextDecoder().decode(pt)) as { provider: string };
  return rec.provider;
}
