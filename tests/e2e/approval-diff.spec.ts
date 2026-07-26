/**
 * approval-diff.spec.ts
 *
 * E2E coverage for #604: the trust-on-change approval gate's pending-diff
 * surface. Reuses the file-change.spec.ts pattern (mutate a fixture task's
 * file in the temp copy at DICODE_E2E_TASKS_DIR, poll the API until the
 * reconciler flips pending_approval) but drives the dashboard afterwards to
 * confirm the "View diff" affordance renders real diff content instead of
 * leaving the operator to approve blind.
 *
 * Uses hello-manual, the same fixture task file-change.spec.ts mutates —
 * safe under this suite's serial (workers: 1, fullyParallel: false)
 * execution as long as the file is restored to its exact original bytes
 * before the test ends (which puts the gate's content hash straight back to
 * the already-approved record, so no explicit re-approval step is needed).
 */

import { test, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { gotoWebui, navigateInSpa, waitForTaskDetail } from './helpers/webui';

const MANUAL_TASK_ID = 'e2e-tests/hello-manual';
const DIFF_MARKER = '// e2e-diff-probe-marker';

// CLAUDE.md documents the reconciler loop as syncing sources "every 30s";
// fsnotify (pkg/taskset/source.go) is meant to make same-machine edits show
// up far faster than that in practice (file-change.spec.ts's equivalent wait
// uses a 20s budget). This test adds a fixed 15s pre-mutation delay (see
// below) to clear the daemon's 10s approval-bootstrap window, plus some
// margin for CPU contention from the fixture set's own per-minute cron/
// storage tasks — mirrors cron.spec.ts's spec-level override for the same
// class of "wait on the real daemon's background loops" test.
test.setTimeout(120_000);

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
  timeoutMs = 60_000,
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

test.describe('Approval pending-diff', () => {
  test('dashboard shows a View diff affordance with real diff content for a pending task', async ({ page, request }) => {
    const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');
    const original = fs.readFileSync(taskJsPath, 'utf8');

    try {
      // The daemon's approval gate holds a fixed bootstrapSettle = 10s
      // first-run seeding window (pkg/daemon/daemon.go) after startup, during
      // which Admit's Bootstrapping() branch auto-approves whatever content
      // is CURRENTLY on disk as the baseline — no "pending" transition. This
      // global-setup fixture's daemon starts fresh (no dicode.lock) for every
      // spec file, so if we mutate the file before that window closes, our
      // marker gets silently baked in as the approved baseline and
      // pending_approval never flips true (confirmed by direct repro: three
      // consecutive real timeouts each fired almost exactly at that run's
      // configured budget, not at some contention-dependent duration — the
      // task only ever re-pended once cleanup's finally block restored the
      // original bytes against that marker-inclusive baseline). Wait out the
      // window with a safety margin before mutating.
      await new Promise((r) => setTimeout(r, 15_000));

      // Append a distinctive, easy-to-assert-on line so the task's content
      // hash changes (re-pending it) and the diff has an unambiguous added
      // line to look for.
      fs.writeFileSync(taskJsPath, original + `\n${DIFF_MARKER}\n`, 'utf8');

      await waitForTaskCondition(
        request,
        MANUAL_TASK_ID,
        (t) => t.pending_approval === true,
      );

      // The pending-diff API itself must report the added line.
      const diffRes = await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/pending-diff`);
      expect(diffRes.ok()).toBe(true);
      const diffBody = await diffRes.json() as {
        files: Array<{ path: string; status: string; unified_diff: string }>;
      };
      const jsFile = diffBody.files.find((f) => f.path === 'task.js');
      expect(jsFile).toBeTruthy();
      expect(jsFile!.unified_diff).toContain(DIFF_MARKER);

      // Now confirm the dashboard surfaces it.
      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);

      // Scope to the badge's own title attribute rather than a bare text
      // match: the task-detail page also streams live daemon log lines (e.g.
      // "... WARN  task held pending approval — tri…"), which contain
      // "pending approval" as a substring and made a plain hasText locator
      // resolve to multiple elements (a real strict-mode failure hit in CI).
      await expect(page.locator('span[title="This task is new or changed and its triggers are not armed until approved"]')).toBeVisible();
      const viewDiffBtn = page.locator('button', { hasText: 'View diff' });
      await expect(viewDiffBtn).toBeVisible();

      await viewDiffBtn.click({ force: true });
      await expect(page.locator('button', { hasText: 'Hide diff' })).toBeVisible();

      // The diff panel fetches asynchronously — wait for the marker text to
      // actually land in the DOM rather than asserting immediately.
      await expect(page.locator('dc-task-detail')).toContainText(DIFF_MARKER, { timeout: 10_000 });
      await expect(page.locator('dc-task-detail')).toContainText('task.js');
    } finally {
      // Restore exact original bytes: the content hash returns to the
      // already-approved record, so the reconciler re-arms it without any
      // explicit re-approval, leaving a clean baseline for later specs.
      fs.writeFileSync(taskJsPath, original, 'utf8');
      await waitForTaskCondition(
        request,
        MANUAL_TASK_ID,
        (t) => t.pending_approval !== true,
      );
    }
  });
});
