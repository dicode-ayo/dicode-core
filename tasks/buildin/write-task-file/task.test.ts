import { assertEquals, assertRejects } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main, { assertTaskFilePath } from "./task.ts";

function sdk(params: Record<string, string>) {
  return {
    params: {
      get: (k: string) => Promise.resolve(params[k] ?? null),
    },
  } as never;
}

async function withTmpDir(fn: (dir: string) => Promise<void>): Promise<void> {
  const dir = await Deno.makeTempDir({ prefix: "write-task-file-test-" });
  try {
    await fn(dir);
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
}

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

Deno.test("rejects_traversal_and_relative_paths", () => {
  rejects("data/ai-tasks/my-task/task.ts", "relative");
  rejects("/data/ai-tasks/my-task/../../task.ts", "parent segment");
  rejects("/data/ai-tasks/my-task/task.ts\0", "NUL byte");
});

Deno.test("writes_a_task_file", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/my-task/task.yaml`;
    const result = await main(sdk({ content: "name: x\n", path, roots: dir }));
    assertEquals(result, { path });
    assertEquals(await Deno.readTextFile(path), "name: x\n");
  });
});

Deno.test("overwrites_an_existing_task_file", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/my-task/task.ts`;
    await Deno.mkdir(`${dir}/my-task`);
    await Deno.writeTextFile(path, "old");
    await main(sdk({ content: "new", path, roots: dir }));
    assertEquals(await Deno.readTextFile(path), "new");
  });
});

Deno.test("refuses_to_write_a_rejected_path", async () => {
  await withTmpDir(async (dir) => {
    await assertRejects(() => main(sdk({ content: "x", path: `${dir}/taskset.yaml`, roots: dir })));
    await assertRejects(() => main(sdk({ path: `${dir}/t/task.ts`, roots: dir })));
  });
});
