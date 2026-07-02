// Deletes orphaned task temp files from /tmp AND orphaned per-task
// scratch directories from ${DATADIR}/tmp/.
//
// 1. /tmp file sweep — each dicode runtime writes its wrapper to a file
//    named  dicode-<kind>-<runID>__<rand>.<ext>  (where <kind> is one
//    of shim | runner | task, <runID> is the UUID assigned by the
//    registry, and the double-underscore separates the run_id from
//    Go's CreateTemp random suffix). A file is considered an orphan
//    iff its embedded run_id is not in the set of currently-running
//    runs. Files whose name does not match the expected shape are
//    left alone.
//
// 2. ${DATADIR}/tmp/<task>/<uuid>/ directory sweep — tasks like
//    buildin/ai-agent-claude-cli mkdir a per-invocation workdir at
//    ${DATADIR}/tmp/<task-name>/<uuid>/ and run a subprocess with cwd
//    pointing there (the .claude/ project-local config goes inside).
//    We sweep any leaf directory whose mtime is older than DIR_TTL
//    (1 hour) — well past the realistic duration of any in-flight
//    invocation, so this never deletes a live workdir. Inline cleanup
//    in the task itself is best-effort; this cron is the source of
//    truth.

const PREFIXES = ["dicode-shim-", "dicode-runner-", "dicode-task-"];
const TEMP_DIR = "/tmp";

// DIR_TTL_MS is the age threshold for the ${DATADIR}/tmp/ sweep.
// Any leaf workdir older than this is removed regardless of whether
// the source task is still registered. 1 hour generously exceeds any
// realistic agent-task duration; tighten if observed leaks accumulate.
const DIR_TTL_MS = 60 * 60 * 1000;

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
  const running = new Set<string>();
  const tasks = (await dicode.list_tasks()) as TaskSummary[];
  for (const t of tasks) {
    // dicode.get_runs returns null (not an empty array) when a task has no
    // runs yet, or when the task was deregistered between list_tasks and
    // get_runs. Coerce to [] so the iteration is always safe.
    const runs = ((await dicode.get_runs(t.id, { limit: 100 })) ?? []) as Run[];
    for (const r of runs) {
      if (r.Status === "running") running.add(r.ID);
    }
  }
  return running;
}

// listDir eagerly drains Deno.readDir into an array. Deno.readDir is lazy:
// NotFound (dir absent) and PermissionDenied (an unreadable subtree) surface
// while iterating, not at the call, so draining inside a try/catch is the only
// way to keep them from escaping as an uncaught rejection. NotFound is a normal
// "nothing to sweep" signal and stays silent; other errors are logged.
async function listDir(dir: string): Promise<Deno.DirEntry[]> {
  const out: Deno.DirEntry[] = [];
  try {
    for await (const e of Deno.readDir(dir)) out.push(e);
  } catch (e) {
    if (!(e instanceof Deno.errors.NotFound)) {
      console.warn("temp-cleanup: readDir", dir, String(e));
    }
  }
  return out;
}

// sweepDataDirTmp removes per-invocation scratch directories under
// ${DATADIR}/tmp/<task>/<uuid>/ that are older than DIR_TTL_MS.
// Returns counters for logging.
async function sweepDataDirTmp(): Promise<{ scanned: number; deleted: number; skipped: number }> {
  // `||` (not `??`) so a declared-but-unset entry arriving as "" falls back
  // rather than resolving root to "/tmp" and sweeping the whole system temp.
  const dataDir = Deno.env.get("DICODE_DATADIR") ||
                  `${Deno.env.get("HOME") ?? "/root"}/.dicode`;
  const root = `${dataDir}/tmp`;
  const cutoff = Date.now() - DIR_TTL_MS;
  let scanned = 0, deleted = 0, skipped = 0;

  for (const taskEntry of await listDir(root)) {
    if (!taskEntry.isDirectory) continue;
    const taskRoot = `${root}/${taskEntry.name}`;
    for (const leaf of await listDir(taskRoot)) {
      if (!leaf.isDirectory) continue;
      const path = `${taskRoot}/${leaf.name}`;
      scanned++;
      let stat: Deno.FileInfo;
      try {
        stat = await Deno.stat(path);
      } catch (e) {
        console.warn("temp-cleanup: stat", path, String(e));
        continue;
      }
      if (stat.mtime && stat.mtime.getTime() > cutoff) {
        // Within TTL — likely a live invocation, skip.
        skipped++;
        continue;
      }
      try {
        await Deno.remove(path, { recursive: true });
        deleted++;
      } catch (e) {
        console.warn("temp-cleanup: remove", path, String(e));
      }
    }
  }
  return { scanned, deleted, skipped };
}

export default async function main({ dicode }: DicodeSdk) {
  const running = await collectRunningRunIDs(dicode);

  let scanned = 0;
  let deleted = 0;
  let skipped = 0;

  for (const entry of await listDir(TEMP_DIR)) {
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
      await Deno.remove(path);
      deleted++;
    } catch (err) {
      console.warn("remove failed", path, String(err));
    }
  }

  // Second sweep: per-invocation scratch dirs under ${DATADIR}/tmp/.
  const dirSweep = await sweepDataDirTmp();

  console.log("temp-cleanup", {
    files:   { scanned, deleted, skipped },
    dirs:    dirSweep,
    running: running.size,
  });
  return {
    files:   { scanned, deleted, skipped },
    dirs:    dirSweep,
    running: running.size,
  };
}
