// Sweeps expired run-input blobs (#233).
//
// Layout: persisted run inputs live under ${DATADIR}/run-inputs/<runID>.bin
// (managed by the configured storage task — typically buildin/local-storage).
// A row is "expired" when input_stored_at < now - retention_seconds AND
// input_pinned = 0.
//
// dicode.runs.list_expired returns the expired rows; dicode.runs.delete_input
// hands the storage task the delete + clears the runs row's input columns.
// This task only orchestrates; the engine does the actual work.

import type { DicodeSdk } from "../../sdk.ts";

interface ExpiredRow {
  RunID: string;
  StorageKey: string;
  StoredAt: number;
}

// deno-lint-ignore no-explicit-any
type DicodeWithRuns = any;

export default async function main({ params, dicode }: DicodeSdk) {
  // dicode.runs.* is injected by the daemon at runtime (Task 11 SDK extension).
  const retentionStr = (await params.get("retention_seconds")) ?? "2592000";
  const retention = Number(retentionStr);
  if (!Number.isFinite(retention) || retention <= 0) {
    return { ok: false, error: `invalid retention_seconds: ${retentionStr}` };
  }

  const cutoff = Math.floor(Date.now() / 1000) - retention;

  const rows = (await (dicode as DicodeWithRuns).runs.list_expired({ before_ts: cutoff })) as
    | ExpiredRow[]
    | null;

  if (!rows || rows.length === 0) {
    return { ok: true, removed: 0, errors: 0 };
  }

  let removed = 0;
  let errors = 0;
  for (const row of rows) {
    try {
      await (dicode as DicodeWithRuns).runs.delete_input(row.RunID);
      removed++;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      console.warn(`delete_input(${row.RunID}) failed: ${msg}`);
      errors++;
    }
  }
  return { ok: errors === 0, removed, errors };
}
