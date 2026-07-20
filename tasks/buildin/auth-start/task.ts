// buildin/auth-start — kicks off an OAuth flow via the dicode relay broker.
//
// Loads the daemon's relay identity (encrypted blob via dicode.crypto +
// the configured storage task), signs an /auth/:provider URL via the
// dicode-relay/client library's buildAuthURL, and returns the URL.
//
// PKCE: a fresh verifier is generated per invocation; only the challenge
// (sha256 of verifier, base64url) is included in the signed payload to
// the broker. The verifier is discarded — the broker performs the PKCE
// exchange with the upstream provider, not the daemon.

import {
  Identity,
  buildAuthURL,
  type StoredIdentity,
} from "npm:dicode-relay@0.2.1/client";

const IDENTITY_CTX   = "dicode/relay-identity/v1";
const PENDING_CTX    = "dicode/oauth-pending/v1";
const PREFIX         = "relay/";
const ID_KEY         = "relay/identity-v1";

function b64decode(s: string): Uint8Array {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

function b64encode(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

function b64urlEncode(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export default async function main({ params, dicode, output }: DicodeSdk) {
  const provider = String((await params.get("provider")) ?? "");
  const scope    = String((await params.get("scope"))    ?? "");
  if (!provider) throw new Error("provider parameter is required");

  const brokerURL = Deno.env.get("DICODE_RELAY_BROKER_URL");
  if (!brokerURL) {
    throw new Error("relay broker URL not configured (DICODE_RELAY_BROKER_URL)");
  }

  const datadir     = Deno.env.get("DICODE_DATADIR") ?? ".";
  const root        = `${datadir}/relay-store`;
  const storageTask = Deno.env.get("DICODE_STORAGE_TASK") ?? "buildin/local-storage";

  // 1. Load identity (blob is encrypted at rest; decrypt happens here).
  const identity = await loadIdentity(dicode, storageTask, root);

  // 2. Generate PKCE verifier + challenge.
  const verifier  = crypto.randomUUID() + crypto.randomUUID();
  const challenge = b64urlEncode(
    new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier))),
  );

  // 3. Build the signed URL.
  const result = await buildAuthURL({
    provider,
    scope:     scope || undefined,
    identity,
    brokerURL,
    challenge,
  });

  // 4. Persist {sessionId → provider} so auth-relay can resolve the provider
  //    on callback. Encrypted at rest for hygiene; not security-critical.
  await dicode.run_task(storageTask, {
    op:     "put",
    key:    `oauth-pending/${result.sessionId}`,
    value:  b64encode(
      await dicode.crypto.encrypt(
        PENDING_CTX,
        new TextEncoder().encode(JSON.stringify({ provider })),
      ),
    ),
    prefix: "oauth-pending/",
    root,
  });

  const lines = [
    `OAuth flow started for ${provider}.`,
    ``,
    `Open this URL in a browser to authorize:`,
    ``,
    `  ${result.url}`,
    ``,
    `Once you complete the provider's consent screen, the dicode relay will`,
    `deliver the encrypted token to this daemon. buildin/auth-relay will`,
    `decrypt it and write the credentials to your secrets store under`,
    `${provider.toUpperCase()}_ACCESS_TOKEN (and _REFRESH_TOKEN, _EXPIRES_AT if applicable).`,
    ``,
    `Session: ${result.sessionId}`,
  ];
  const html = `<pre>${lines.map(escapeHtml).join("\n")}</pre>`;
  await output.html(html);
  return { url: result.url, session_id: result.sessionId };
}

// dicode.run_task returns a RunResult envelope: { runID, status, returnValue }.
// Unwrap it to get the storage task's actual return value.
function unwrapRunResult(raw: unknown): { ok: boolean; value?: string; error?: string } {
  const envelope = raw as { returnValue?: unknown };
  const rv = envelope?.returnValue ?? raw;
  return rv as { ok: boolean; value?: string; error?: string };
}

async function loadIdentity(
  dicode: DicodeSdk["dicode"],
  storageTask: string,
  root: string,
): Promise<Identity> {
  const res = unwrapRunResult(await dicode.run_task(storageTask, {
    op: "get",
    key: ID_KEY,
    prefix: PREFIX,
    root,
  }));

  if (!res.ok || !res.value) {
    throw new Error(
      "relay identity not found in storage — has the relay-client task started yet?",
    );
  }
  const ct = b64decode(res.value);
  const pt = await dicode.crypto.decrypt(IDENTITY_CTX, ct);
  const stored = JSON.parse(new TextDecoder().decode(pt)) as StoredIdentity;
  return await Identity.import(stored);
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#x27;");
}
