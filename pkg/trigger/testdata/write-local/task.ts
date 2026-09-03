// buildin/write-local — generic file-write library task.
//
// Writes a string to a file at a given path with a given mode. Returns
// the resolved path. Designed to be the bottom-of-pipe step after
// buildin/template renders a config — the rendered string flows in via
// ${input.output} interpolation on the caller's per-edge overrides,
// and we persist it where the consuming daemon (or external process)
// expects to read it.
//
// Permissions intentionally ship empty (fs: []). Every call site
// declares its own fs:rw scope via overrides.fs, mirroring
// buildin/template's loud-failure / explicit-scoping contract.

import type { DicodeSdk } from "../sdk.ts";

interface WriteResult {
  path: string;
}

// parseMode accepts octal strings like "0600", "600", "0644". Anything
// outside [0-7]{3} (with an optional leading 0) is rejected loudly: a
// silent widening of file mode on a secret-bearing config (rendered
// relay.yaml, cloudflared credentials, etc.) is a real-world footgun.
// The 3-digit cap also blocks the setuid/setgid/sticky bits — a
// rendered config has no business carrying 4xxx/2xxx/1xxx modes.
export function parseMode(s: string): number {
  if (!/^0?[0-7]{3}$/.test(s)) {
    throw new Error(`invalid mode: ${JSON.stringify(s)} (expected octal like "0600")`);
  }
  return parseInt(s, 8);
}

export default async function main(
  { params }: DicodeSdk,
): Promise<WriteResult> {
  const content = await params.get("content");
  if (content === null) {
    throw new Error("missing required param: content");
  }
  const path = await params.get("path");
  if (!path) {
    throw new Error("missing required param: path");
  }
  const modeStr = (await params.get("mode")) ?? "0600";
  const mode = parseMode(modeStr);

  // Require an absolute path. task.yaml documents this contract; we
  // enforce it loudly here rather than relying on the fs:rw allowlist
  // to reject relative paths at write time with a cryptic error.
  if (!path.startsWith("/")) {
    throw new Error(`invalid path: must be absolute (got ${JSON.stringify(path)})`);
  }
  if (path.includes("/../") || path.endsWith("/..") || path === "..") {
    throw new Error(`invalid path: parent-directory segments not allowed (${JSON.stringify(path)})`);
  }
  // Block embedded NUL bytes before they reach Deno.writeTextFile. The
  // fs:rw allowlist already confines us to declared roots — this is
  // defence in depth that surfaces caller-config typos as a loud error
  // rather than a cryptic OS-level reject.
  if (path.includes("\0")) {
    throw new Error(`invalid path: contains NUL byte`);
  }

  // Auto-create the parent directory. Allowed only inside the fs:rw
  // root the caller declared via overrides.fs — Deno's --allow-write
  // sandbox enforces this regardless of what we attempt here. Path is
  // guaranteed absolute by the check above, so lastSlash >= 0 always.
  const lastSlash = path.lastIndexOf("/");
  if (lastSlash > 0) {
    const parent = path.slice(0, lastSlash);
    await Deno.mkdir(parent, { recursive: true });
  }

  // Atomic write: write to a sibling temp file then rename into place.
  // The rename is atomic on the same filesystem (always true here since
  // tmpPath shares path's parent), so concurrent readers (e.g.,
  // cloudflared sighup-reloading its config) never observe a partial
  // write. The UUID suffix prevents collisions when two write-local
  // invocations race on the same target.
  const tmpPath = `${path}.tmp.${crypto.randomUUID()}`;
  try {
    await Deno.writeTextFile(tmpPath, content, { mode });
    await Deno.rename(tmpPath, path);
  } catch (e) {
    // Best-effort cleanup: if the rename failed, the tmp file is
    // orphaned. Swallow cleanup errors (the original is what matters).
    try {
      await Deno.remove(tmpPath);
    } catch {
      // ignore
    }
    throw e;
  }
  return { path };
}
