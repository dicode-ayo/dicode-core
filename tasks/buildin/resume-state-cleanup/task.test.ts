/**
 * task.test.ts — unit tests for resume-state-cleanup (#570).
 *
 * Run with:  make test-tasks
 *            deno test --allow-read --allow-write tasks/buildin/resume-state-cleanup/task.test.ts
 *
 * Mirrors tasks/buildin/run-inputs-cleanup/task.test.ts's structure — the
 * harness's freshDicode() does not include a `runs` sub-object, so each test
 * patches (globalThis as any).dicode.runs before calling runTask().
 */
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

// Helper: wire a minimal dicode.runs mock before each runTask() call.
// deno-lint-ignore no-explicit-any
function setRunsMock(mock: { list_expired_resume_state: any; delete_resume_state: any }) {
  // deno-lint-ignore no-explicit-any
  (globalThis as any).dicode.runs = mock;
}

test("removes expired rows", async () => {
  const expired = [
    { RunID: "r1", StorageKey: "resume-state/r1", StoredAt: 100 },
    { RunID: "r2", StorageKey: "resume-state/r2", StoredAt: 100 },
  ];
  const deleted: string[] = [];
  setRunsMock({
    list_expired_resume_state: async () => expired,
    delete_resume_state: async (runID: string) => {
      deleted.push(runID);
      return { ok: true };
    },
  });

  params.set("retention_seconds", "86400");
  const res = await runTask() as { ok: boolean; removed: number; errors: number };
  assert.equal(res.ok, true);
  assert.equal(res.removed, 2);
  assert.equal(res.errors, 0);
  assert.equal(deleted.length, 2);
  assert.ok(deleted.includes("r1"), "r1 must have been deleted");
  assert.ok(deleted.includes("r2"), "r2 must have been deleted");
});

test("returns ok with 0 when nothing to clean", async () => {
  setRunsMock({
    list_expired_resume_state: async () => [],
    delete_resume_state: async () => ({ ok: true }),
  });

  params.set("retention_seconds", "86400");
  const res = await runTask() as { ok: boolean; removed: number; errors: number };
  assert.equal(res.ok, true);
  assert.equal(res.removed, 0);
  assert.equal(res.errors, 0);
});

test("returns ok with 0 when list_expired_resume_state returns null", async () => {
  setRunsMock({
    list_expired_resume_state: async () => null,
    delete_resume_state: async () => ({ ok: true }),
  });

  params.set("retention_seconds", "86400");
  const res = await runTask() as { ok: boolean; removed: number; errors: number };
  assert.equal(res.ok, true);
  assert.equal(res.removed, 0);
  assert.equal(res.errors, 0);
});

test("counts errors when delete_resume_state throws", async () => {
  setRunsMock({
    list_expired_resume_state: async () => [
      { RunID: "good", StorageKey: "resume-state/good", StoredAt: 100 },
      { RunID: "bad",  StorageKey: "resume-state/bad",  StoredAt: 100 },
    ],
    delete_resume_state: async (runID: string) => {
      if (runID === "bad") throw new Error("storage backend down");
      return { ok: true };
    },
  });

  params.set("retention_seconds", "86400");
  const res = await runTask() as { ok: boolean; removed: number; errors: number };
  assert.equal(res.ok, false);
  assert.equal(res.removed, 1);
  assert.equal(res.errors, 1);
});

test("rejects invalid retention_seconds (non-numeric)", async () => {
  setRunsMock({
    list_expired_resume_state: async () => [],
    delete_resume_state: async () => ({ ok: true }),
  });

  params.set("retention_seconds", "not-a-number");
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(
    res.error && res.error.includes("invalid retention_seconds"),
    `expected error message, got: ${JSON.stringify(res.error)}`,
  );
});

test("rejects zero retention_seconds", async () => {
  setRunsMock({
    list_expired_resume_state: async () => [],
    delete_resume_state: async () => ({ ok: true }),
  });

  params.set("retention_seconds", "0");
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(
    res.error && res.error.includes("invalid retention_seconds"),
    `expected error message for zero, got: ${JSON.stringify(res.error)}`,
  );
});

test("rejects negative retention_seconds", async () => {
  setRunsMock({
    list_expired_resume_state: async () => [],
    delete_resume_state: async () => ({ ok: true }),
  });

  params.set("retention_seconds", "-3600");
  const res = await runTask() as { ok: boolean; error?: string };
  assert.equal(res.ok, false);
  assert.ok(
    res.error && res.error.includes("invalid retention_seconds"),
    `expected error message for negative, got: ${JSON.stringify(res.error)}`,
  );
});

test("uses default retention_seconds from task.yaml when param not set", async () => {
  // The harness seeds paramDefaults from task.yaml; don't call params.set here.
  let capturedBeforeTs: number | undefined;
  setRunsMock({
    list_expired_resume_state: async ({ before_ts }: { before_ts: number }) => {
      capturedBeforeTs = before_ts;
      return [];
    },
    delete_resume_state: async () => ({ ok: true }),
  });

  const res = await runTask() as { ok: boolean; removed: number; errors: number };
  assert.equal(res.ok, true);
  assert.equal(res.removed, 0);
  assert.equal(res.errors, 0);
  // cutoff should be roughly now - 86400; sanity-check it's in the past
  const nowSec = Math.floor(Date.now() / 1000);
  assert.ok(
    capturedBeforeTs !== undefined && capturedBeforeTs < nowSec,
    `before_ts ${capturedBeforeTs} should be in the past`,
  );
});

test("continues deleting remaining rows after a failure", async () => {
  // Verifies the error accumulation loop doesn't short-circuit on failure.
  const deleted: string[] = [];
  setRunsMock({
    list_expired_resume_state: async () => [
      { RunID: "a", StorageKey: "resume-state/a", StoredAt: 100 },
      { RunID: "b", StorageKey: "resume-state/b", StoredAt: 100 },
      { RunID: "c", StorageKey: "resume-state/c", StoredAt: 100 },
    ],
    delete_resume_state: async (runID: string) => {
      if (runID === "b") throw new Error("transient error");
      deleted.push(runID);
      return { ok: true };
    },
  });

  params.set("retention_seconds", "86400");
  const res = await runTask() as { ok: boolean; removed: number; errors: number };
  assert.equal(res.ok, false);
  assert.equal(res.removed, 2);
  assert.equal(res.errors, 1);
  // "a" and "c" must both have been processed despite "b" failing
  assert.ok(deleted.includes("a"), "a must have been deleted");
  assert.ok(deleted.includes("c"), "c must have been deleted after b failed");
});
