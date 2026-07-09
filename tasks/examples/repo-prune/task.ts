import type { DicodeSdk } from "../../sdk.ts";

/**
 * Analyse → approve → prune stale git worktrees and branches.
 *
 * main() runs the script read-only and suspends with the plan. resume() feeds
 * that same plan back with --apply --plan, so the approved bytes are the
 * executed bytes.
 */

const SCRIPT = new URL("./prune-stale-refs.sh", import.meta.url).pathname;

interface Plan {
  summary: {
    worktrees_delete: number;
    worktrees_keep: number;
    local_delete: number;
    local_keep: number;
    remote_delete: number;
  };
  worktrees: { delete: { path: string; branch: string }[] };
  local_branches: { delete: string[] };
  remote_branches: { delete: string[] };
}

async function sh(args: string[], cwd: string): Promise<string> {
  const cmd = new Deno.Command("bash", { args: [SCRIPT, ...args], cwd, stdout: "piped", stderr: "piped" });
  const { code, stdout, stderr } = await cmd.output();
  const out = new TextDecoder().decode(stdout);
  const err = new TextDecoder().decode(stderr).trim();
  if (code !== 0) {
    throw new Error(`prune-stale-refs.sh ${args.join(" ")} exited ${code}: ${err || "(no stderr)"}`);
  }
  if (err) console.warn(`prune script stderr: ${err}`);
  return out;
}

/** A one-line-per-ref preview, capped: a 250-branch list in a form is unreadable. */
function preview(names: string[], cap = 12): string {
  if (names.length === 0) return "(none)";
  const head = names.slice(0, cap).join("\n");
  return names.length > cap ? `${head}\n… and ${names.length - cap} more` : head;
}

export default async function main({ params, dicode }: DicodeSdk) {
  const repo = (await params.get("repo_path")) ?? "";
  const includeLocked = (await params.get("include_locked")) === "true";

  console.log(`repo-prune: analysing ${repo} (include_locked=${includeLocked})`);
  const plan: Plan = JSON.parse(await sh(includeLocked ? ["--include-locked"] : [], repo));
  const s = plan.summary;

  const total = s.worktrees_delete + s.local_delete + s.remote_delete;
  if (total === 0) {
    console.log("repo-prune: nothing stale");
    return { applied: false, reason: "nothing to prune", summary: s };
  }

  const worktrees = plan.worktrees.delete.map((w) => w.path);
  console.log(`repo-prune: ${total} stale ref(s) — pausing for approval`);

  await dicode.suspend({
    state: { plan, repo },
    // A destructive plan should not wait forever for a human.
    deadline: Date.now() + 24 * 60 * 60 * 1000,
    schema: {
      type: "object",
      title: `Prune ${total} stale ref(s) from ${repo}?`,
      description:
        `Worktrees: ${s.worktrees_delete} delete / ${s.worktrees_keep} keep\n` +
        `Local branches: ${s.local_delete} delete / ${s.local_keep} keep\n` +
        `Remote branches: ${s.remote_delete} delete\n\n` +
        `Worktrees:\n${preview(worktrees)}\n\n` +
        `Local:\n${preview(plan.local_branches.delete)}\n\n` +
        `Remote:\n${preview(plan.remote_branches.delete)}\n\n` +
        `Remote deletions are restorable from each merged PR's page on GitHub.`,
      properties: {
        approve: { type: "boolean", title: "Execute this plan", default: false },
        note: { type: "string", title: "Note (recorded in the run log)", default: "" },
      },
      required: ["approve"],
    },
  });
  // Unreachable — suspend() never returns.
}

export async function resume({ input, state }: DicodeSdk) {
  const { approve, note = "" } = input as { approve: boolean; note?: string };
  const { plan, repo } = state as { plan: Plan; repo: string };

  if (!approve) {
    console.log(`repo-prune: declined${note ? ` — ${note}` : ""}`);
    return { applied: false, reason: "declined by operator", note };
  }

  // Hand the approved plan back on disk rather than re-analysing: between the
  // suspend and the resume the repo may have moved, and re-deriving would widen
  // the blast radius past what was shown. Lives under .git/ so it is covered by
  // the task's fs grant and never lands in a working tree.
  const planPath = `${repo}/.git/prune-plan.json`;
  await Deno.writeTextFile(planPath, JSON.stringify(plan));
  try {
    const out = await sh(["--apply", "--plan", planPath], repo);
    console.log(`repo-prune: applied${note ? ` — ${note}` : ""}`);
    return { applied: true, note, summary: plan.summary, output: out.trim().split("\n") };
  } finally {
    await Deno.remove(planPath).catch(() => {});
  }
}
