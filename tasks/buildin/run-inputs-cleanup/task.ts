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

// Field names are the JSON tags on registry.ExpiredInput, not its Go field
// names — the daemon marshals RunID as "runID".
interface ExpiredRow {
  runID: string;
  storageKey: string;
  storedAt: number;
}

// deno-lint-ignore no-explicit-any
type DicodeWithRuns = any;

export default async function main({ params, dicode }: DicodeSdk) {
  // dicode.runs.* is injected by the daemon at runtime.
  const retentionStr = (await params.get("retention_seconds")) ?? "2592000";
  const retention = Number(retentionStr);
  if (!Number.isFinite(retention) || retention <= 0) {
    return { ok: false, error: `invalid retention_seconds: ${retentionStr}` };
  }

  // A backlog can exceed what one tick can delete inside task.yaml's timeout,
  // and being killed mid-sweep reports failure for work that partly succeeded.
  // Stop at the budget instead and let the next tick continue.
  const budgetStr = (await params.get("budget_seconds")) ?? "90";
  const budget = Number(budgetStr);
  if (!Number.isFinite(budget) || budget <= 0) {
    return { ok: false, error: `invalid budget_seconds: ${budgetStr}` };
  }
  const deadline = Date.now() + budget * 1000;

  const cutoff = Math.floor(Date.now() / 1000) - retention;

  const rows = (await (dicode as DicodeWithRuns).runs.list_expired({ before_ts: cutoff })) as
    | ExpiredRow[]
    | null;

  if (!rows || rows.length === 0) {
    return { ok: true, removed: 0, errors: 0, remaining: 0 };
  }

  let removed = 0;
  let errors = 0;
  let remaining = 0;
  for (const [i, row] of rows.entries()) {
    if (Date.now() >= deadline) {
      remaining = rows.length - i;
      console.log(`budget of ${budget}s reached; ${remaining} left for the next tick`);
      break;
    }
    try {
      await (dicode as DicodeWithRuns).runs.delete_input(row.runID);
      removed++;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      console.warn(`delete_input(${row.runID}) failed: ${msg}`);
      errors++;
    }
  }
  return { ok: errors === 0, removed, errors, remaining };
}
