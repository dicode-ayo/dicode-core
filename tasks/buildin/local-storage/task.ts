// Local Storage backend for run-input persistence (#233).
//
// Stores base64-encoded ciphertext blobs as files under a fixed root.
// Core does encryption/redaction; this task is a dumb byte store.

interface PutResult { ok: true }
interface GetResult { ok: true; value: string }
interface DeleteResult { ok: true }
interface ErrorResult { ok: false; error: string }

const KEY_PREFIX = "run-inputs/";

function fileFor(root: string, key: string): string {
  // Key is "run-inputs/<runID>" by convention. Strip the prefix; the
  // remainder must be a single safe path component.
  if (!key.startsWith(KEY_PREFIX)) {
    throw new Error(`storage key must start with ${JSON.stringify(KEY_PREFIX)}: ${key}`);
  }
  const safeKey = key.slice(KEY_PREFIX.length);
  if (!safeKey || safeKey.includes("/") || safeKey.includes("\\") || safeKey.includes("..")) {
    throw new Error(`invalid storage key: ${key}`);
  }
  return `${root}/${safeKey}.bin`;
}

function base64Decode(s: string): Uint8Array {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

function base64Encode(b: Uint8Array): string {
  let s = "";
  for (const byte of b) s += String.fromCharCode(byte);
  return btoa(s);
}

export default async function main({ params }: DicodeSdk):
  Promise<PutResult | GetResult | DeleteResult | ErrorResult> {
  const op = String((await params.get("op")) ?? "");
  const key = String((await params.get("key")) ?? "");
  const root = String((await params.get("root")) ?? "");

  if (!op || !key) return { ok: false, error: "op and key required" };
  if (!root) return { ok: false, error: "root required (set DATADIR)" };

  try {
    await Deno.mkdir(root, { recursive: true });
    const path = fileFor(root, key);

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
