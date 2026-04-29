/**
 * task.test.ts — unit tests for dev-clones-cleanup.
 *
 * Run with:  make test-tasks
 *
 * Tests use real temp directories so Deno.readDir / Deno.remove are exercised
 * without mocking (--allow-all is passed by the make target). The
 * DICODE_DATA_DIR env var is intercepted via the sdk-test harness so task.ts
 * picks up the test-specific tmpdir.
 *
 * The task constructs its root as ${DICODE_DATA_DIR}/dev-clones. Tests set
 * DICODE_DATA_DIR to a tmpdir that acts as the mock data-dir, then create
 * the dev-clones sub-tree inside it.
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

// Helper: create the dev-clones tree under <dataDir>/dev-clones/<source>/<runID>/.
async function makeCloneTree(
  dataDir: string,
  entries: Array<{ source: string; runID: string }>,
): Promise<void> {
  for (const { source, runID } of entries) {
    await Deno.mkdir(`${dataDir}/dev-clones/${source}/${runID}`, { recursive: true });
  }
}

// Helper: list all run-ID dirs under <dataDir>/dev-clones, flattened.
async function listCloneDirs(dataDir: string): Promise<string[]> {
  const root = `${dataDir}/dev-clones`;
  const result: string[] = [];
  try {
    for await (const sourceEntry of Deno.readDir(root)) {
      if (!sourceEntry.isDirectory) continue;
      for await (const runEntry of Deno.readDir(`${root}/${sourceEntry.name}`)) {
        if (runEntry.isDirectory) result.push(runEntry.name);
      }
    }
  } catch (e) {
    if (!(e instanceof Deno.errors.NotFound)) throw e;
  }
  return result.sort();
}

test("removes orphan clones, keeps active run", async () => {
  // dataDir acts as the mock DICODE_DATA_DIR; the task appends /dev-clones.
  const dataDir = await Deno.makeTempDir({ prefix: "dc-cleanup-test-" });
  try {
    await makeCloneTree(dataDir, [
      { source: "buildin", runID: "run-active-1" },
      { source: "buildin", runID: "run-orphan-1" },
      { source: "myrepo",  runID: "run-orphan-2" },
    ]);

    env.set("DICODE_DATA_DIR", dataDir);

    // Stub get_runs: "auto-fix" returns one running run; others return empty.
    dicode.get_runs = async (id: string) => {
      if (id === "auto-fix") {
        return [
          { ID: "run-active-1", Status: "running" },
          { ID: "run-done-1",   Status: "success" },
        ];
      }
      return [];
    };

    const result = await runTask() as { ok: boolean; removed: number; kept: number };

    assert.equal(result.ok, true);
    assert.equal(result.removed, 2);
    assert.equal(result.kept, 1);

    // Active clone must survive; orphans must be gone.
    const remaining = await listCloneDirs(dataDir);
    assert.equal(JSON.stringify(remaining), JSON.stringify(["run-active-1"]));
  } finally {
    await Deno.remove(dataDir, { recursive: true });
  }
});

test("returns early with note when dev-clones dir does not exist", async () => {
  const dataDir = await Deno.makeTempDir({ prefix: "dc-cleanup-ne-" });
  // Do NOT create dev-clones under it — task should detect NotFound.

  env.set("DICODE_DATA_DIR", dataDir);

  let getRunsCalled = 0;
  dicode.get_runs = async () => { getRunsCalled++; return []; };

  try {
    const result = await runTask() as { ok: boolean; note?: string };

    assert.equal(result.ok, true);
    assert.ok(
      result.note?.includes("no dev-clones dir"),
      `expected note about missing dir, got: ${JSON.stringify(result.note)}`,
    );
  } finally {
    await Deno.remove(dataDir, { recursive: true });
  }
});

test("swallows task-not-found error from get_runs for unknown task IDs", async () => {
  const dataDir = await Deno.makeTempDir({ prefix: "dc-cleanup-err-" });
  try {
    await makeCloneTree(dataDir, [
      { source: "s1", runID: "run-xyz" },
    ]);

    env.set("DICODE_DATA_DIR", dataDir);

    // Simulate all auto-fix task IDs returning "task not found".
    dicode.get_runs = async () => {
      throw new Error("task not found: auto-fix");
    };

    const result = await runTask() as { ok: boolean; removed: number };

    // All clones are orphans (no active runs could be collected).
    assert.equal(result.ok, true);
    assert.equal(result.removed, 1);
  } finally {
    await Deno.remove(dataDir, { recursive: true });
  }
});

test("non-directory entries under source dir are ignored", async () => {
  const dataDir = await Deno.makeTempDir({ prefix: "dc-cleanup-nondir-" });
  try {
    await Deno.mkdir(`${dataDir}/dev-clones/mysource`, { recursive: true });
    // A plain file sitting alongside the run dirs should be skipped.
    await Deno.writeTextFile(`${dataDir}/dev-clones/mysource/not-a-run.txt`, "stale file");
    await Deno.mkdir(`${dataDir}/dev-clones/mysource/real-run`, { recursive: true });

    env.set("DICODE_DATA_DIR", dataDir);
    dicode.get_runs = async () => [];

    const result = await runTask() as { ok: boolean; removed: number; kept: number };

    assert.equal(result.ok, true);
    // Only the "real-run" dir counts; the .txt file is silently skipped.
    assert.equal(result.removed, 1);
    assert.equal(result.kept, 0);
  } finally {
    await Deno.remove(dataDir, { recursive: true });
  }
});
