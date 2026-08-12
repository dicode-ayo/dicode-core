// Sweeps expired offloaded resume-state blobs (#570).
//
// Layout: offloaded resume-state blobs live under
// ${DATADIR}/resume-state/<runID>.bin (managed by the configured storage
// task — typically buildin/local-storage; a separate root/prefix from
// run-inputs so the two mechanisms never collide).
//
// A row is "expired" (sweep-eligible) when resume_state_stored_at < now -
// retention_seconds AND the row is no longer `suspended` — see
// Registry.ListExpiredResumeStates for the full safety rationale (a
// still-suspended row's blob is a live reference a future resume will
// dereference, so it is never returned by list_expired_resume_state
// regardless of age).
//
// dicode.runs.list_expired_resume_state returns the expired rows;
// dicode.runs.delete_resume_state hands the storage task the delete + clears
// the runs row's offload columns. This task only orchestrates; the engine
// does the actual work.

import type { DicodeSdk } from "../../sdk.ts";

interface ExpiredResumeStateRow {
  RunID: string;
  StorageKey: string;
  StoredAt: number;
}

// deno-lint-ignore no-explicit-any
type DicodeWithRuns = any;

export default async function main({ params, dicode }: DicodeSdk) {
  // dicode.runs.* is injected by the daemon at runtime.
  const retentionStr = (await params.get("retention_seconds")) ?? "86400";
  const retention = Number(retentionStr);
  if (!Number.isFinite(retention) || retention <= 0) {
    return { ok: false, error: `invalid retention_seconds: ${retentionStr}` };
  }

  const cutoff = Math.floor(Date.now() / 1000) - retention;

  const rows = (await (dicode as DicodeWithRuns).runs.list_expired_resume_state({
    before_ts: cutoff,
  })) as ExpiredResumeStateRow[] | null;

  if (!rows || rows.length === 0) {
    return { ok: true, removed: 0, errors: 0 };
  }

  let removed = 0;
  let errors = 0;
  for (const row of rows) {
    try {
      await (dicode as DicodeWithRuns).runs.delete_resume_state(row.RunID);
      removed++;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      console.warn(`delete_resume_state(${row.RunID}) failed: ${msg}`);
      errors++;
    }
  }
  return { ok: errors === 0, removed, errors };
}
