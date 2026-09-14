/**
 * file-status.spec.ts
 *
 * E2E coverage for #670's per-file "what moved" markers: the pending-review
 * file inventory (`GET /api/tasks/{id}/pending-state`) decorates each entry
 * with an optional `status` of "new" or "changed", computed by comparing git
 * blob hashes between the previously-approved commit and the one the
 * pending content was observed at (pkg/approval/state.go's inventoryOf,
 * internal/gitops.TreeBlobHashesForPaths). This is the per-file half of the
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

test.setTimeout(90_000);

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
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await request.get(`/api/tasks/${encodeURIComponent(taskID)}`);
    if (res.ok()) {
      const body = await res.json() as Record<string, unknown>;
      if (predicate(body)) return;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`Task ${taskID} did not satisfy condition within ${timeoutMs}ms`);
}

type PendingStateFile = { path: string; kind: string; status?: string };
type PendingState = { pending_hash: string; files?: PendingStateFile[] };

test.describe('Per-file "what moved" markers (#670)', () => {
  test('pending-state marks an edited file changed, a new file new, and leaves an untouched file unmarked', async ({ request }) => {
    const repo = tasksDir();
    const probeDir = path.join(repo, 'file-status-probe');
    const taskJsPath = path.join(probeDir, 'task.js');
    const newFilePath = path.join(probeDir, 'extra.js');
    const git = (...args: string[]): string =>
      execFileSync('git', args, { cwd: repo, encoding: 'utf8' }).trim();

    // file-status-probe is baked into the daemon's very first commit
    // (initFixtureRepo) and bootstrap-approves at startup, exactly like every
    // other fixture task. But pkg/daemon's approval-bootstrap window
    // (bootstrapSettle, nominally 10s but slid forward by every task
    // registration that lands while it's open — see daemon.go) auto-approves
    // ANY hash change observed before it closes instead of holding it
    // pending, so mutating too early would bake this test's edit in as
    // "already approved" and pending_approval would never flip true — the
    // exact trap approval-review.spec.ts's withPendingChange guards against
    // with the same fixed delay. This spec has no guarantee it runs after
    // another spec file has already outlived the window (Playwright file
    // order, or a filtered/solo run of just this file — as this comment's
    // own author confirmed against a real run), so wait it out unconditionally
    // rather than only on a "first test in the file" heuristic.
    await new Promise((r) => setTimeout(r, 15_000));

    const original = fs.readFileSync(taskJsPath, 'utf8');
    const edited = original.replace('baseline', 'edited-for-e2e-670');
    expect(edited, 'fixture task.js must contain the literal "baseline" marker this test edits').not.toBe(original);

    try {
      fs.writeFileSync(taskJsPath, edited, 'utf8');
      fs.writeFileSync(newFilePath, 'export const probe = true;\n', 'utf8');
      // A REAL commit — see the file header on why an empty one won't do —
      // so the pending "to" commit's tree actually carries these bytes.
      git('add', 'file-status-probe');
      git('commit', '-q', '-m', 'file-status-probe: edit + add for #670 e2e');

      await waitForTaskCondition(request, TASK_ID, (t) => t.pending_approval === true);

      const res = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}/pending-state`);
      expect(res.ok(), await res.text()).toBe(true);
      const state = await res.json() as PendingState;

      const byPath = new Map((state.files ?? []).map((f) => [f.path, f]));

      const editedFile = byPath.get('task.js');
      expect(editedFile, `task.js missing from inventory: ${JSON.stringify(state.files)}`).toBeTruthy();
      expect(editedFile!.status).toBe('changed');

      const addedFile = byPath.get('extra.js');
      expect(addedFile, `extra.js missing from inventory: ${JSON.stringify(state.files)}`).toBeTruthy();
      expect(addedFile!.status).toBe('new');

      // task.yaml was never touched by this test: no `status` key at all
      // (omitempty), not an explicit "" or "unchanged" value.
      const untouchedFile = byPath.get('task.yaml');
      expect(untouchedFile, `task.yaml missing from inventory: ${JSON.stringify(state.files)}`).toBeTruthy();
      expect(untouchedFile).not.toHaveProperty('status');

      // No code bytes anywhere in the payload, same invariant every other
      // pending-state spec in this suite pins.
      expect(JSON.stringify(state)).not.toContain('edited-for-e2e-670');
    } finally {
      // The commit above is a legitimate, permanent step for this dedicated
      // fixture — unlike hello-manual's mutate/restore convention elsewhere
      // in this suite, there is no byte-identical-content revert path this
      // test relies on. Settling to approved is what leaves the daemon in
      // the state every later spec assumes: no task left pending.
      try {
        await settleApproved(request, TASK_ID);
      } catch (cleanupError) {
        console.error('file-status-probe cleanup failed:', cleanupError);
      }
    }
  });
});
