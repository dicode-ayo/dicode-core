/**
 * file-status.spec.ts
 *
 * E2E coverage for #670's per-file "what moved" markers: the pending-review
 * file inventory (`GET /api/tasks/{id}/pending-state`) decorates each entry
 * with an optional `status` of "new" or "changed", computed by comparing git
 * blob hashes between the previously-approved commit and the one the
 * pending content was observed at (pkg/approval/state.go's inventoryOf,
 * internal/gitops.TreeBlobHashesForPathsAtTwoCommits). This is the per-file half of the
 * "what moved" strip; the commit-range/compare-link half (#846, superseding
 * #672) already has e2e coverage in approval-review.spec.ts.
 *
 * Reuses the git-backed e2e fixture source every approval spec in this suite
 * runs against (tests/e2e/helpers/dicode-server.ts's initFixtureRepo makes
 * DICODE_E2E_TASKS_DIR a real git repository with an `origin` remote), but
 * against a DEDICATED fixture task (file-status-probe) rather than
 * hello-manual: every other approval spec here mutates a fixture's working
 * tree and restores the exact original bytes, relying on git HEAD moving (if
 * at all) via an *empty* commit — see approval-review.spec.ts's compare-link
 * test. Markers need the opposite: a REAL commit whose tree actually holds
 * the new bytes (an empty commit reuses the parent's tree unchanged, so
 * nothing would ever resolve to "new"/"changed"). Giving that its own
 * fixture keeps a genuinely content-changing commit from touching any other
 * spec's assumptions about hello-manual's history.
 */

import { test, expect } from '@playwright/test';
import { execFileSync } from 'child_process';
import * as fs from 'fs';
import * as path from 'path';
import { settleApproved } from './helpers/approval';

const TASK_ID = 'e2e-tests/file-status-probe';
const MARKER_PREFIX = 'edited-for-e2e-670';

// Retrying the mutate-and-check-pending sequence (see attemptPendingChange)
// can burn through several backed-off waits in a slow/loaded CI run before
// pkg/daemon's approval-bootstrap window finally closes — give the whole
// test enough budget to try all of them plus cleanup.
test.setTimeout(240_000);

function tasksDir(): string {
  const d = process.env.DICODE_E2E_TASKS_DIR;
  if (!d) throw new Error('DICODE_E2E_TASKS_DIR not set — global setup may have failed');
  return d;
}

/** Poll GET /api/tasks/{id} until the predicate is satisfied, up to timeoutMs. */
async function waitForTaskCondition(
  request: import('@playwright/test').APIRequestContext,
  taskID: string,
  predicate: (task: Record<string, unknown>) => boolean,
  timeoutMs = 45_000,
): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await request.get(`/api/tasks/${encodeURIComponent(taskID)}`);
    if (res.ok()) {
      const body = await res.json() as Record<string, unknown>;
      if (predicate(body)) return true;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  return false;
}

type PendingStateFile = { path: string; kind: string; status?: string };
type PendingState = { pending_hash: string; files?: PendingStateFile[] };

type PendingAttempt = {
  ok: boolean;
  /** The distinctive marker this attempt's edit wrote into task.js. */
  marker: string;
  /** The distinctive new file this attempt added, relative to probeDir. */
  newFile: string;
};

/**
 * attemptPendingChange edits task.js and adds a new file, commits both, and
 * waits up to pendTimeoutMs for the task to report pending_approval === true.
 *
 * Each attempt uses a fresh marker/filename so a run that gets swallowed by
 * pkg/daemon's approval-bootstrap window (bootstrapSettle, nominally 10s but
 * slid forward by every task registration that lands while it's open — see
 * daemon.go) auto-approving the change instead of holding it pending still
 * leaves the NEXT attempt's edit and new file genuinely novel relative to
 * whatever just got auto-approved as the new baseline — rather than retrying
 * with identical content that would just get silently re-approved again with
 * nothing left to observe as "new"/"changed".
 */
async function attemptPendingChange(
  request: import('@playwright/test').APIRequestContext,
  git: (...args: string[]) => string,
  probeDir: string,
  taskJsPath: string,
  attempt: number,
  pendTimeoutMs: number,
): Promise<PendingAttempt> {
  const marker = `${MARKER_PREFIX}-${attempt}`;
  const newFile = `extra-${attempt}.js`;

  const original = fs.readFileSync(taskJsPath, 'utf8');
  // The first attempt replaces the fixture's literal "baseline" marker (see
  // its own assertion below); later attempts just append, since "baseline"
  // is gone after the first edit lands.
  const edited = original.includes('baseline')
    ? original.replace('baseline', marker)
    : `${original}\n// ${marker}\n`;

  fs.writeFileSync(taskJsPath, edited, 'utf8');
  fs.writeFileSync(path.join(probeDir, newFile), `export const probe = ${attempt};\n`, 'utf8');
  // A REAL commit — see the file header on why an empty one won't do — so
  // the pending "to" commit's tree actually carries these bytes.
  git('add', 'file-status-probe');
  git('commit', '-q', '-m', `file-status-probe: edit + add for #670 e2e (attempt ${attempt})`);

  const ok = await waitForTaskCondition(request, TASK_ID, (t) => t.pending_approval === true, pendTimeoutMs);
  return { ok, marker, newFile };
}

test.describe('Per-file "what moved" markers (#670)', () => {
  test('pending-state marks an edited file changed, a new file new, and leaves an untouched file unmarked', async ({ request }) => {
    const repo = tasksDir();
    const probeDir = path.join(repo, 'file-status-probe');
    const taskJsPath = path.join(probeDir, 'task.js');
    const git = (...args: string[]): string =>
      execFileSync('git', args, { cwd: repo, encoding: 'utf8' }).trim();

    const fixtureOriginal = fs.readFileSync(taskJsPath, 'utf8');
    expect(fixtureOriginal, 'fixture task.js must contain the literal "baseline" marker this test edits')
      .toContain('baseline');

    // file-status-probe is baked into the daemon's very first commit
    // (initFixtureRepo) and bootstrap-approves at startup, exactly like every
    // other fixture task. The bootstrap window's nominal 10s duration slides
    // forward with every task registration that lands while it's open, so
    // there is no single fixed wait that is guaranteed to outlast it under a
    // loaded CI run with many fixture tasks — instead of one fixed sleep
    // followed by a single check (which would fail with a confusing timeout
    // on `pending_approval === true` if the window was still open), retry
    // the mutate-commit-check sequence a few times with backoff so a slow
    // run gets more chances to observe the window finally closed.
    const backoffMs = [15_000, 20_000, 30_000, 30_000];
    let result: PendingAttempt | undefined;
    for (let attempt = 1; attempt <= backoffMs.length; attempt++) {
      await new Promise((r) => setTimeout(r, backoffMs[attempt - 1]));
      result = await attemptPendingChange(request, git, probeDir, taskJsPath, attempt, 20_000);
      if (result.ok) break;
    }

    try {
      expect(
        result?.ok,
        `${TASK_ID} never reported pending_approval=true across ${backoffMs.length} attempts — ` +
          'the approval-bootstrap window kept auto-approving every edit before it could be observed pending',
      ).toBe(true);
      const { marker, newFile } = result!;

      const res = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}/pending-state`);
      expect(res.ok(), await res.text()).toBe(true);
      const state = await res.json() as PendingState;

      const byPath = new Map((state.files ?? []).map((f) => [f.path, f]));

      const editedFile = byPath.get('task.js');
      expect(editedFile, `task.js missing from inventory: ${JSON.stringify(state.files)}`).toBeTruthy();
      expect(editedFile!.status).toBe('changed');

      const addedFile = byPath.get(newFile);
      expect(addedFile, `${newFile} missing from inventory: ${JSON.stringify(state.files)}`).toBeTruthy();
      expect(addedFile!.status).toBe('new');

      // task.yaml was never touched by this test: no `status` key at all
      // (omitempty), not an explicit "" or "unchanged" value.
      const untouchedFile = byPath.get('task.yaml');
      expect(untouchedFile, `task.yaml missing from inventory: ${JSON.stringify(state.files)}`).toBeTruthy();
      expect(untouchedFile).not.toHaveProperty('status');

      // No code bytes anywhere in the payload, same invariant every other
      // pending-state spec in this suite pins.
      expect(JSON.stringify(state)).not.toContain(marker);
    } finally {
      // Every attempt's commit above is a legitimate, permanent step for this
      // dedicated fixture — unlike hello-manual's mutate/restore convention
      // elsewhere in this suite, there is no byte-identical-content revert
      // path this test relies on. Settling to approved is what leaves the
      // daemon in the state every later spec assumes: no task left pending.
      try {
        await settleApproved(request, TASK_ID);
      } catch (cleanupError) {
        console.error('file-status-probe cleanup failed:', cleanupError);
      }
    }
  });
});
