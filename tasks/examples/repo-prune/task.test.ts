import { setupHarness } from "../../sdk-test.ts";
import { resume } from "./task.ts";
await setupHarness(import.meta.url);

// suspend isn't part of the default mock surface; capture the request and throw
// so the handler under test stops where the real runtime would exit.
function captureSuspend() {
  const calls: Record<string, unknown>[] = [];
  const suspend = (req: Record<string, unknown>) => {
    calls.push(req);
    throw new Error("__suspended__");
  };
  return { calls, suspend };
}

const EMPTY_PLAN = {
  summary: { worktrees_delete: 0, worktrees_keep: 0, local_delete: 0, local_keep: 3, remote_delete: 0 },
  worktrees: { delete: [] },
  local_branches: { delete: [] },
  remote_branches: { delete: [] },
};

const FULL_PLAN = {
  summary: { worktrees_delete: 1, worktrees_keep: 2, local_delete: 2, local_keep: 5, remote_delete: 1 },
  worktrees: { delete: [{ path: "/repo/.wt/a", branch: "feat/a" }] },
  local_branches: { delete: ["feat/a", "feat/b"] },
  remote_branches: { delete: ["feat/a"] },
};

const origCommand = Deno.Command;

/** Replace Deno.Command so no subprocess ever runs; record each argv. */
function stubCommand(stdout: string, code = 0): string[][] {
  const seen: string[][] = [];
  const enc = new TextEncoder();
  (Deno as unknown as Record<string, unknown>).Command = class {
    #args: string[];
    constructor(_bin: string, opts: { args: string[] }) {
      this.#args = opts.args;
    }
    output() {
      seen.push(this.#args);
      return Promise.resolve({ code, stdout: enc.encode(stdout), stderr: enc.encode("") });
    }
  };
  return seen;
}

function restore() {
  (Deno as unknown as Record<string, unknown>).Command = origCommand;
}

test("nothing stale: returns early and never suspends", async () => {
  const { calls, suspend } = captureSuspend();
  (dicode as Record<string, unknown>).suspend = suspend;
  stubCommand(JSON.stringify(EMPTY_PLAN));
  params.set("repo_path", "/repo");
  params.set("include_locked", "false");

  const result = await runTask() as { applied: boolean; reason: string };

  assert.equal(calls.length, 0);
  assert.equal(result.applied, false);
  assert.equal(result.reason, "nothing to prune");
  restore();
});

test("a non-empty plan suspends for approval and carries the plan in state", async () => {
  const { calls, suspend } = captureSuspend();
  (dicode as Record<string, unknown>).suspend = suspend;
  stubCommand(JSON.stringify(FULL_PLAN));
  params.set("repo_path", "/repo");
  params.set("include_locked", "false");

  let threw = false;
  try {
    await runTask();
  } catch {
    threw = true;
  }

  assert.ok(threw);
  assert.equal(calls.length, 1);
  const req = calls[0] as {
    schema: { required: string[]; title: string };
    state: { plan: typeof FULL_PLAN };
  };
  assert.equal(req.schema.required[0], "approve");
  // The plan the operator approves is the plan resume replays.
  assert.equal(req.state.plan.local_branches.delete.length, 2);
  // 1 worktree + 2 local + 1 remote.
  assert.ok(req.schema.title.includes("4"), `want total 4 in title, got: ${req.schema.title}`);
  restore();
});

test("include_locked is forwarded to the script", async () => {
  const { suspend } = captureSuspend();
  (dicode as Record<string, unknown>).suspend = suspend;
  const seen = stubCommand(JSON.stringify(EMPTY_PLAN));
  params.set("repo_path", "/repo");
  params.set("include_locked", "true");

  await runTask();

  assert.ok(seen[0].includes("--include-locked"));
  restore();
});

test("declining the plan applies nothing", async () => {
  const seen = stubCommand("");

  const result = await resume({
    input: { approve: false, note: "not now" },
    state: { plan: FULL_PLAN, repo: "/repo" },
  } as never) as { applied: boolean; reason: string };

  assert.equal(result.applied, false);
  assert.equal(result.reason, "declined by operator");
  assert.equal(seen.length, 0);
  restore();
});

test("approving replays the approved plan verbatim via --apply --plan", async () => {
  const seen = stubCommand("worktree removed: /repo/.wt/a\ndone");
  const writes: Record<string, string> = {};
  const origWrite = Deno.writeTextFile;
  const origRemove = Deno.remove;
  (Deno as unknown as Record<string, unknown>).writeTextFile = (p: string, c: string) => {
    writes[p] = c;
    return Promise.resolve();
  };
  (Deno as unknown as Record<string, unknown>).remove = () => Promise.resolve();

  const result = await resume({
    input: { approve: true },
    state: { plan: FULL_PLAN, repo: "/repo" },
  } as never) as { applied: boolean };

  assert.equal(result.applied, true);
  assert.ok(seen[0].includes("--apply"));
  assert.ok(seen[0].includes("--plan"), "replays a plan file rather than re-analysing");
  assert.equal(JSON.parse(writes["/repo/.git/prune-plan.json"]).local_branches.delete.length, 2);

  (Deno as unknown as Record<string, unknown>).writeTextFile = origWrite;
  (Deno as unknown as Record<string, unknown>).remove = origRemove;
  restore();
});

test("a failing script surfaces its stderr rather than a bare exit code", async () => {
  const { suspend } = captureSuspend();
  (dicode as Record<string, unknown>).suspend = suspend;
  (Deno as unknown as Record<string, unknown>).Command = class {
    constructor(_bin: string, _opts: unknown) {}
    output() {
      return Promise.resolve({
        code: 1,
        stdout: new TextEncoder().encode(""),
        stderr: new TextEncoder().encode("refusing: plan targets a protected branch: main"),
      });
    }
  };
  params.set("repo_path", "/repo");
  params.set("include_locked", "false");

  let msg = "";
  try {
    await runTask();
  } catch (e) {
    msg = (e as Error).message;
  }
  assert.ok(msg.includes("protected branch"), `want stderr in the error, got: ${msg}`);
  restore();
});
