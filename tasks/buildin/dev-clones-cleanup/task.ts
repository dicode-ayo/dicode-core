// Sweeps orphaned dev-mode clone directories.
//
// Layout: ${DATADIR}/dev-clones/<sourceName>/<runID>/
// A clone is orphan iff its <runID> is not in the set of currently-running
// auto-fix runs. Files/dirs that do not fit the layout are left alone.
//
// Path resolution (in preference order):
//   1. DICODE_DATA_DIR env var (injected via permissions.env; covers Docker
//      deployments with a non-default data_dir)
//   2. params.dev_clones_root (defaults to ${HOME}/.dicode/dev-clones at
//      load time via the built-in HOME template variable)

interface Run {
  ID: string;
  Status: string;
}

// Candidate task IDs for the auto-fix feature (#238). Tasks not yet registered
// produce a "task not found" error from dicode.get_runs; we swallow those and
// continue so the cleanup still works once any one of them is live.
const AUTO_FIX_TASK_IDS = ["auto-fix", "auto-fix-review", "auto-fix-autonomous"];

async function collectActiveRunIDs(dicode: Dicode): Promise<Set<string>> {
  const active = new Set<string>();
  for (const id of AUTO_FIX_TASK_IDS) {
    let runs: Run[] = [];
    try {
      runs = (await dicode.get_runs(id, { limit: 100 })) as Run[];
    } catch {
      // Task not yet registered (auto-fix #238 not shipped) — skip silently.
      continue;
    }
    for (const r of runs) {
      if (r.Status === "running") active.add(r.ID);
    }
  }
  return active;
}

export default async function main({ params, dicode }: DicodeSdk): Promise<unknown> {
  // Prefer the DICODE_DATA_DIR env var (set in Docker / explicit deploys) so we
  // always operate on the actual data directory regardless of the ~/.dicode default.
  const dataEnv = Deno.env.get("DICODE_DATA_DIR");
  const root = dataEnv
    ? `${dataEnv}/dev-clones`
    : (await params.get("dev_clones_root")) ?? "";

  if (!root) {
    dicode.log?.error?.("could not determine dev-clones root — DICODE_DATA_DIR unset and dev_clones_root param empty");
    return { ok: false, error: "dev_clones_root unset" };
  }

  const active = await collectActiveRunIDs(dicode);

  let removed = 0;
  let kept = 0;

  // List source-name directories under <root>/.
  let sourceEntries: Deno.DirEntry[] = [];
  try {
    for await (const entry of Deno.readDir(root)) {
      sourceEntries.push(entry);
    }
  } catch (e) {
    if (e instanceof Deno.errors.NotFound) {
      // Directory does not exist yet — no clones have been created; nothing to do.
      return { ok: true, removed: 0, kept: 0, note: "no dev-clones dir yet" };
    }
    throw e;
  }

  // For each <sourceName>/<runID>/ entry, remove the clone if its run is no
  // longer active.
  for (const sourceEntry of sourceEntries) {
    if (!sourceEntry.isDirectory) continue;
    const sourcePath = `${root}/${sourceEntry.name}`;
    for await (const runEntry of Deno.readDir(sourcePath)) {
      if (!runEntry.isDirectory) continue;
      const clonePath = `${sourcePath}/${runEntry.name}`;
      if (active.has(runEntry.name)) {
        kept++;
        continue;
      }
      try {
        await Deno.remove(clonePath, { recursive: true });
        removed++;
        console.log(`dev-clones-cleanup: removed orphan ${clonePath}`);
      } catch (err) {
        console.warn(`dev-clones-cleanup: failed to remove ${clonePath}: ${String(err)}`);
      }
    }
  }

  console.log("dev-clones-cleanup", { root, removed, kept, active: active.size });
  return { ok: true, removed, kept };
}
