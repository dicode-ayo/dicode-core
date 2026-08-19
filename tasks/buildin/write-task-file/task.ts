// buildin/write-task-file — writes one file of a dicode task.
//
// The caller of this task is a language model choosing both the path and the
// content, so the fs:rw grant in the taskset entry is the outer boundary and
// this file is the inner one: a write is only ever one of a task's own files,
// inside a directory of its own.

import type { DicodeSdk } from "../../sdk.ts";

interface WriteResult {
  path: string;
}

// The files a dicode task directory is made of. A taskset file is absent on
// purpose: its entries may carry a git ref whose auth.token_env the daemon
// reads from its own environment and sends to that ref's URL on the next
// reconcile, with no approval in between.
const TASK_FILES = new Set([
  "task.yaml",
  "task.yml",
  "task.ts",
  "task.js",
  "task.py",
  "task.test.ts",
  "task.test.js",
  "task.test.py",
  "deno.json",
  "deno.lock",
  "task.py.lock",
  "requirements.txt",
  "pyproject.toml",
  "Dockerfile",
  "README.md",
]);

// assertTaskFilePath throws unless path names a file of a task in a directory
// of its own under one of roots. <root>/<task>/<file> is the shape the
// resolver reads: a file directly in the source root belongs to the source,
// not to any task, so the depth is a boundary and not a formality.
export function assertTaskFilePath(path: string, roots: string[]): void {
  if (!path.startsWith("/")) {
    throw new Error(`invalid path: must be absolute (got ${JSON.stringify(path)})`);
  }
  if (path.includes("\0")) {
    throw new Error("invalid path: contains NUL byte");
  }
  const root = roots.find((r) => path.startsWith(r.replace(/\/+$/, "") + "/"));
  if (!root) {
    throw new Error(
      `invalid path: ${JSON.stringify(path)} is outside the writable roots (${roots.join(", ")})`,
    );
  }
  const segments = path.slice(root.replace(/\/+$/, "").length + 1).split("/");
  if (segments.length < 2) {
    throw new Error(
      `invalid path: ${JSON.stringify(path)} — a task's files live in a directory of their own, not in the source root`,
    );
  }
  for (const segment of segments) {
    if (segment === "" || segment === "." || segment === "..") {
      throw new Error(`invalid path: empty or relative segment in ${JSON.stringify(path)}`);
    }
    if (segment === ".git") {
      throw new Error(`invalid path: ${JSON.stringify(path)} reaches into a git directory`);
    }
  }
  const name = segments[segments.length - 1];
  if (!TASK_FILES.has(name)) {
    throw new Error(
      `invalid path: ${JSON.stringify(name)} is not a task file — allowed: ${[...TASK_FILES].join(", ")}`,
    );
  }
}

export default async function main({ params }: DicodeSdk): Promise<WriteResult> {
  const content = await params.get("content");
  if (content === null) {
    throw new Error("missing required param: content");
  }
  const path = await params.get("path");
  if (!path) {
    throw new Error("missing required param: path");
  }
  const roots = ((await params.get("roots")) ?? "")
    .split(",")
    .map((r) => r.trim())
    .filter(Boolean);
  if (roots.length === 0) {
    throw new Error("missing required param: roots");
  }
  assertTaskFilePath(path, roots);

  await Deno.mkdir(path.slice(0, path.lastIndexOf("/")), { recursive: true });

  // Write to a sibling temp file and rename into place, so the reconciler
  // never resolves a half-written task file.
  const tmpPath = `${path}.tmp.${crypto.randomUUID()}`;
  try {
    await Deno.writeTextFile(tmpPath, content, { mode: 0o644 });
    await Deno.rename(tmpPath, path);
  } catch (e) {
    try {
      await Deno.remove(tmpPath);
    } catch {
      // the original error is what matters
    }
    throw e;
  }
  return { path };
}
