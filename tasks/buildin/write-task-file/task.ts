// buildin/write-task-file — writes one file of a dicode task.
//
// The caller of this task is a language model choosing both the path and the
// content, so the fs:rw grant in the taskset entry is the outer boundary and
// this file is the inner one: a write is only ever one of a task's own files,
// inside a directory of its own.

import { parseAll as parseYamlAll } from "jsr:@std/yaml@1";

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
  // Exactly <root>/<task>/<file>: the shape the resolver reads. Shallower is
  // the source root, which belongs to the source rather than to any task;
  // deeper reaches subtrees the content hash skips (node_modules, .git), where
  // an edit would not re-pend an already-approved task.
  const segments = path.slice(root.replace(/\/+$/, "").length + 1).split("/");
  if (segments.length !== 2) {
    throw new Error(
      `invalid path: ${JSON.stringify(path)} — a task's files live directly in a directory of their own under ${root}`,
    );
  }
  for (const segment of segments) {
    if (segment === "" || segment === "." || segment === "..") {
      throw new Error(`invalid path: empty or relative segment in ${JSON.stringify(path)}`);
    }
  }
  const name = segments[segments.length - 1];
  if (!TASK_FILES.has(name)) {
    throw new Error(
      `invalid path: ${JSON.stringify(name)} is not a task file — allowed: ${[...TASK_FILES].join(", ")}`,
    );
  }
}

// assertTaskDocument throws unless content is a task manifest. The resolver
// decides what a file is by reading its `kind`, not by its name: a task.yaml
// declaring `kind: TaskSet` is resolved as one, and a taskset entry may carry
// a git ref whose auth.token_env the daemon reads from its own environment and
// sends to that ref's URL on the next reconcile — no approval in between.
export function assertTaskDocument(path: string, content: string): void {
  if (!path.endsWith("/task.yaml") && !path.endsWith("/task.yml")) {
    return;
  }
  // U+0085, U+2028 and U+2029 are line breaks in YAML 1.1 and ordinary
  // characters in 1.2. The daemon's parser honours them and this one does
  // not, so a manifest containing them splits into different documents on
  // each side — and the daemon's side is the one that decides what the file
  // is. A task manifest has no need for them.
  const split = content.match(/[\u0085\u2028\u2029]/);
  if (split) {
    throw new Error(
      `invalid task.yaml: contains U+${split[0].codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")}, ` +
        `which the daemon's YAML parser reads as a line break and this one does not`,
    );
  }
  let docs: unknown[];
  try {
    docs = parseYamlAll(content) as unknown[];
  } catch (e) {
    throw new Error(`invalid task.yaml: ${e instanceof Error ? e.message : String(e)}`);
  }
  if (docs.length !== 1) {
    throw new Error(`invalid task.yaml: expected one document, got ${docs.length}`);
  }
  const kind = (docs[0] as Record<string, unknown> | null)?.kind;
  if (kind !== "Task") {
    throw new Error(
      `invalid task.yaml: kind is ${JSON.stringify(kind ?? null)}, and this tool writes tasks only`,
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
  // Not a param: an ai-agent exposes every declared param to the model as a
  // tool argument, which would put the boundary in the caller's hands.
  const roots = (Deno.env.get("DICODE_TASK_FILE_ROOTS") ?? "")
    .split(",")
    .map((r) => r.trim())
    .filter(Boolean);
  if (roots.length === 0) {
    throw new Error("DICODE_TASK_FILE_ROOTS is not set — the task has no writable root");
  }
  assertTaskFilePath(path, roots);
  assertTaskDocument(path, content);

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
