// Deletes orphaned task temp files from /tmp.
//
// Each dicode runtime writes its wrapper to a file named
//   dicode-<kind>-<runID>__<rand>.<ext>
// where <kind> is one of shim | runner | task, <runID> is the UUID
// assigned by the registry, and the double-underscore separates the
// run_id from Go's CreateTemp random suffix (UUIDs contain dashes, so
// a single dash would be ambiguous).
//
// A file is considered an orphan iff its embedded run_id is not in the
// set of currently-running runs. Files whose name does not match the
// expected shape are left alone.

const PREFIXES = ["dicode-shim-", "dicode-runner-", "dicode-task-"];
const TEMP_DIR = "/tmp";

interface TaskSummary {
  id: string;
}

interface Run {
  ID: string;
  Status: string;
}

function parseRunID(name: string): string | null {
  for (const prefix of PREFIXES) {
    if (!name.startsWith(prefix)) continue;
    const rest = name.slice(prefix.length);
    const sep = rest.indexOf("__");
    if (sep <= 0) return null;
    return rest.slice(0, sep);
  }
  return null;
}

async function collectRunningRunIDs(dicode: Dicode): Promise<Set<string>> {
  // ListRuns returns a nil slice when a task has no runs, which serializes
  // to JSON null — coerce to [] so the for-of loops are safe.
  const running = new Set<string>();
  const tasks = ((await dicode.list_tasks()) as TaskSummary[] | null) ?? [];
  for (const t of tasks) {
    const runs =
      ((await dicode.get_runs(t.id, { limit: 100 })) as Run[] | null) ?? [];
    for (const r of runs) {
      if (r.Status === "running") running.add(r.ID);
    }
  }
  return running;
}

export default async function main({ dicode }: DicodeSdk) {
  // Record scan start before any registry query. Files created after this
  // point belong to runs that may not appear in our running set (race with
  // task launch); we protect them via an mtime lower bound.
  const scanStart = Date.now();
  const running = await collectRunningRunIDs(dicode);

  let scanned = 0;
  let deleted = 0;
  let skipped = 0;
  let tooNew = 0;

  for await (const entry of Deno.readDir(TEMP_DIR)) {
    if (!entry.isFile) continue;
    const runID = parseRunID(entry.name);
    if (runID === null) continue;
    scanned++;
    if (running.has(runID)) {
      skipped++;
      continue;
    }
    const path = `${TEMP_DIR}/${entry.name}`;
    try {
      const stat = await Deno.stat(path);
      if (stat.mtime && stat.mtime.getTime() >= scanStart) {
        // File was created or touched after we queried the registry — a run
        // may have started for it that we never saw. Leave it for next cycle.
        tooNew++;
        continue;
      }
      await Deno.remove(path);
      deleted++;
    } catch (err) {
      console.warn("remove failed", path, String(err));
    }
  }

  const summary = { scanned, deleted, skipped, tooNew, running: running.size };
  console.log("temp-cleanup", summary);
  return summary;
}
