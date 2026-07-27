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
// withPendingChange's preMutateDelayMs) to clear the daemon's 10s approval-
// bootstrap window, plus some margin for CPU contention from the fixture
// set's own per-minute cron/storage tasks — mirrors cron.spec.ts's spec-
// level override for the same class of "wait on the real daemon's
// background loops" test.
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

/**
 * withPendingChange appends DIFF_MARKER to hello-manual/task.js, waits for
 * the gate to hold it pending, runs body, then — always, even on failure —
 * restores the original bytes and waits for pending_approval to clear again.
 * Shared mutate/wait/restore scaffolding for every test in this file.
 *
 * preMutateDelayMs, when > 0, waits out the daemon's fixed 10s approval-
 * bootstrap window (pkg/daemon/daemon.go's bootstrapSettle) before writing.
 * During that window Admit's Bootstrapping() branch auto-approves whatever
 * content is CURRENTLY on disk as the baseline, so mutating too early bakes
 * the marker in as "already approved" and pending_approval never flips true
 * (confirmed by direct repro: three consecutive real timeouts each fired
 * almost exactly at that run's configured budget, not at some contention-
 * dependent duration — the task only ever re-pended once cleanup's finally
 * block restored the original bytes against that marker-inclusive
 * baseline). Only the first test in this serial-execution file needs this —
 * by the time later tests run, the daemon has long since cleared the
 * window.
 */
async function withPendingChange(
  request: import('@playwright/test').APIRequestContext,
  body: () => Promise<void>,
  preMutateDelayMs = 0,
): Promise<void> {
  const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');
  const original = fs.readFileSync(taskJsPath, 'utf8');

  try {
    if (preMutateDelayMs > 0) {
      await new Promise((r) => setTimeout(r, preMutateDelayMs));
    }
    // Append a distinctive, easy-to-assert-on line so the task's content
    // hash changes (re-pending it) and the diff has an unambiguous added
    // line to look for.
    fs.writeFileSync(taskJsPath, original + `\n${DIFF_MARKER}\n`, 'utf8');
    await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval === true);

    await body();
  } finally {
    // Restore exact original bytes: the content hash returns to the
    // already-approved record, so the reconciler re-arms it without any
    // explicit re-approval, leaving a clean baseline for later specs.
    fs.writeFileSync(taskJsPath, original, 'utf8');
    await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true);
  }
}

test.describe('Approval pending-diff', () => {
  test('dashboard shows a View diff affordance with real diff content for a pending task', async ({ page, request }) => {
    await withPendingChange(request, async () => {
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
    }, 15_000);
  });

  test('the task list cannot approve — it hands off to the detail page gate', async ({ page, request }) => {
    await withPendingChange(request, async () => {
      await gotoWebui(page);

      // A table row has nowhere to show a diff, so the row must not offer
      // approval at all — otherwise it is a one-click bypass of the gate.
      await expect(page.locator('dc-task-list button', { hasText: 'Review' }).first()).toBeVisible();
      await expect(page.locator('dc-task-list button', { hasText: 'Approve' })).toHaveCount(0);

      await page.locator('dc-task-list button', { hasText: 'Review' }).first().click({ force: true });
      await waitForTaskDetail(page);
      await expect(page.locator('button', { hasText: 'Review & approve' })).toBeVisible();

      const afterHandoff = await (
        await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)
      ).json() as Record<string, unknown>;
      expect(afterHandoff.pending_approval).toBe(true);
    });
  });

  test('Approve reveals the diff first and only approves on a second, explicit click', async ({ page, request }) => {
    await withPendingChange(request, async () => {
      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);

      const reviewBtn = page.locator('button', { hasText: 'Review & approve' });
      const confirmBtn = page.locator('button', { hasText: 'Confirm approve' });
      await expect(reviewBtn).toBeVisible();

      // The whole point of the gate: the first click must not approve.
      await reviewBtn.click({ force: true });
      await expect(page.locator('dc-task-detail')).toContainText(DIFF_MARKER, { timeout: 10_000 });
      await expect(confirmBtn).toBeVisible();
      const afterFirstClick = await (
        await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)
      ).json() as Record<string, unknown>;
      expect(afterFirstClick.pending_approval).toBe(true);

      // Hiding the diff must disarm — approval cannot be confirmed against a
      // change the operator has collapsed back out of view.
      await page.locator('button', { hasText: 'Hide diff' }).click({ force: true });
      await expect(reviewBtn).toBeVisible();

      await reviewBtn.click({ force: true });
      await expect(confirmBtn).toBeVisible();
      await confirmBtn.click({ force: true });

      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true);
      // Restoring the original bytes in withPendingChange's finally returns
      // the content hash to the record approved by the confirm click above
      // (or by bootstrap, if the test failed before it), so the task is
      // left armed either way.
    });
  });
});
