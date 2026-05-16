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

import type { DicodeSdk } from "../../sdk.ts";

interface WriteResult {
  path: string;
}

// parseMode accepts octal strings like "0600", "600", "0644". Anything
// outside [0-7]{3,4} is rejected loudly: a silent widening of file mode
// on a secret-bearing config (rendered relay.yaml, cloudflared
// credentials, etc.) is a real-world footgun.
export function parseMode(s: string): number {
  if (!/^0?[0-7]{3,4}$/.test(s)) {
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

  // Block embedded NUL bytes before they reach Deno.writeTextFile. The
  // fs:rw allowlist already confines us to declared roots — this is
  // defence in depth that surfaces caller-config typos as a loud error
  // rather than a cryptic OS-level reject.
  if (path.includes("\0")) {
    throw new Error(`invalid path: contains NUL byte`);
  }

  // Auto-create the parent directory. Allowed only inside the fs:rw
  // root the caller declared via overrides.fs — Deno's --allow-write
  // sandbox enforces this regardless of what we attempt here.
  const lastSlash = path.lastIndexOf("/");
  if (lastSlash > 0) {
    const parent = path.slice(0, lastSlash);
    await Deno.mkdir(parent, { recursive: true });
  }

  await Deno.writeTextFile(path, content, { mode });
  return { path };
}
