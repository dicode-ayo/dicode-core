// Blob Storage (user data) — generic filesystem-backed blob store for user
// tasks (#244). Sibling to buildin/local-storage, which stays reserved for
// core's own encrypted run-input/relay-identity persistence.
//
// Layout on disk: ${root}/${namespace}/${key}.bin
//
// Namespace and key are each validated as a single safe path component (no
// separators, no traversal) so a caller cannot escape its own namespace
// directory or another namespace's directory via a crafted key.

interface PutResult { ok: true }
interface GetResult { ok: true; value: string }
interface DeleteResult { ok: true }
interface ListResult { ok: true; keys: string[] }
interface ErrorResult { ok: false; error: string }

const BLOB_EXT = ".bin";

function assertSafeComponent(label: string, value: string): void {
  if (!value || value.includes("/") || value.includes("\\") || value.includes("..")) {
    throw new Error(`invalid ${label}: ${JSON.stringify(value)}`);
  }
}

function namespaceDir(root: string, namespace: string): string {
  assertSafeComponent("namespace", namespace);
  return `${root}/${namespace}`;
}

function fileFor(root: string, namespace: string, key: string): string {
  assertSafeComponent("key", key);
  return `${namespaceDir(root, namespace)}/${key}${BLOB_EXT}`;
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
  Promise<PutResult | GetResult | DeleteResult | ListResult | ErrorResult> {
  const op = String((await params.get("op")) ?? "");
  const namespace = String((await params.get("namespace")) ?? "");
  const key = String((await params.get("key")) ?? "");
  const root = String((await params.get("root")) ?? "");

  if (!op) return { ok: false, error: "op required" };
  if (!root) return { ok: false, error: "root required (set DATADIR)" };

  try {
    if (op === "list") {
      const dir = namespaceDir(root, namespace);
      const keys: string[] = [];
      try {
        for await (const entry of Deno.readDir(dir)) {
          if (entry.isFile && entry.name.endsWith(BLOB_EXT)) {
            keys.push(entry.name.slice(0, -BLOB_EXT.length));
          }
        }
      } catch (e) {
        if (!(e instanceof Deno.errors.NotFound)) throw e;
      }
      keys.sort();
      return { ok: true, keys };
    }

    if (!key) return { ok: false, error: "key required" };
    const path = fileFor(root, namespace, key);

    if (op === "put") {
      const value = String((await params.get("value")) ?? "");
      if (!value) return { ok: false, error: "value required for put" };
      // Decode→write round-trip implicitly validates base64.
      const bytes = base64Decode(value);
      await Deno.mkdir(namespaceDir(root, namespace), { recursive: true });
      await Deno.writeFile(path, bytes);
      return { ok: true };
    }
    if (op === "get") {
      try {
        const bytes = await Deno.readFile(path);
        return { ok: true, value: base64Encode(bytes) };
      } catch (e) {
        if (e instanceof Deno.errors.NotFound) {
          // Treat missing-key as ok with empty value, mirroring local-storage.
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
