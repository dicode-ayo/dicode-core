// Local Storage — generic filesystem-backed blob store.
//
// Stores base64-encoded ciphertext blobs as files under a configurable
// root. The required `prefix` param scopes keys to a domain (e.g.
// "run-inputs/" for #233 input persistence, "relay/" for relay-client
// identity). Core encryption/redaction happens upstream; this task is
// a dumb byte store.

interface PutResult { ok: true }
interface GetResult { ok: true; value: string }
interface DeleteResult { ok: true }
interface ErrorResult { ok: false; error: string }

function fileFor(root: string, prefix: string, key: string): string {
  // Strip the caller-supplied prefix; the remainder must be a single safe path component.
  if (!prefix.endsWith("/")) {
    throw new Error(`prefix must end with '/': ${JSON.stringify(prefix)}`);
  }
  if (!key.startsWith(prefix)) {
    throw new Error(`storage key must start with ${JSON.stringify(prefix)}: ${key}`);
  }
  const safeKey = key.slice(prefix.length);
  if (!safeKey || safeKey.includes("/") || safeKey.includes("\\") || safeKey.includes("..")) {
    throw new Error(`invalid storage key: ${key}`);
  }
  return `${root}/${safeKey}.bin`;
}

function base64Decode(s: string): Uint8Array {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

function base64Encode(b: Uint8Array): string {
  // Build the binary string in chunks. A per-byte `s += ...` loop is
  // quadratic for multi-MB blobs and blows past the task timeout; the
  // subarray+apply form is linear. The chunk stays well under the
  // argument-count limit of String.fromCharCode.
  const CHUNK = 0x8000;
  let s = "";
  for (let i = 0; i < b.length; i += CHUNK) {
    s += String.fromCharCode(...b.subarray(i, i + CHUNK));
  }
  return btoa(s);
}

export default async function main({ params }: DicodeSdk):
  Promise<PutResult | GetResult | DeleteResult | ErrorResult> {
  const op = String((await params.get("op")) ?? "");
  const key = String((await params.get("key")) ?? "");
  const root = String((await params.get("root")) ?? "");
  const prefix = String((await params.get("prefix")) ?? "run-inputs/");

  if (!op || !key) return { ok: false, error: "op and key required" };
  if (!root) return { ok: false, error: "root required (set DATADIR)" };

  try {
    await Deno.mkdir(root, { recursive: true });
    const path = fileFor(root, prefix, key);

    if (op === "put") {
      const value = String((await params.get("value")) ?? "");
      if (!value) return { ok: false, error: "value required for put" };
      // Decode→encode round-trip implicitly validates base64.
      const bytes = base64Decode(value);
      await Deno.writeFile(path, bytes);
      return { ok: true };
    }
    if (op === "get") {
      try {
        const bytes = await Deno.readFile(path);
        return { ok: true, value: base64Encode(bytes) };
      } catch (e) {
        if (e instanceof Deno.errors.NotFound) {
          // Treat missing-key as ok with empty value (caller sees ErrInputUnavailable).
          return { ok: true, value: "" };
        }
        throw e;
      }
    }
    if (op === "delete") {
      try {
        await Deno.remove(path);
      } catch (e) {
        if (!(e instanceof Deno.errors.NotFound)) throw e;
      }
      return { ok: true };
    }
    return { ok: false, error: `unknown op: ${op}` };
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : String(e) };
  }
}
