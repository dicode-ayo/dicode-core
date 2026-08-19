/**
 * pending-task-list-signals.spec.ts
 *
 * E2E coverage for #650: the task list must not present a pending
 * (unapproved) task as though its triggers were live and healthy. Drives
 * the real daemon by mutating hello-webhook's fixture file to flip
 * pending_approval, then asserts the list's toggle/trigger/Run signals read
 * as held rather than healthy, that a pending count/filter exists, and that
 * the notification tray surfaces the transition (the approval:pending
 * WebSocket event that already existed but reached no UI).
 *
 * Reuses hello-webhook (also driven by webhooks.spec.ts) — safe under this
 * suite's serial (workers: 1, fullyParallel: false) execution as long as
 * the file is restored to its exact original bytes before the test ends,
 * per the pattern documented in approval-review.spec.ts.
 */

import { test, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { gotoWebui } from './helpers/webui';
import { withGuaranteedCleanup } from './helpers/cleanup';

const WEBHOOK_TASK_ID = 'e2e-tests/hello-webhook';

test.setTimeout(60_000);

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
  timeoutMs = 30_000,
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
 * withPendingWebhookChange appends a marker comment to hello-webhook's
 * task.js, waits for the gate to hold it pending, runs body, then — always,
 * even on failure — restores the original bytes so later specs (and
 * webhooks.spec.ts's own use of this fixture) see it armed again. Cleanup
 * failures are preserved via withGuaranteedCleanup rather than swallowed.
 */
async function withPendingWebhookChange(
  request: import('@playwright/test').APIRequestContext,
  body: () => Promise<void>,
): Promise<void> {
  const taskJsPath = path.join(tasksDir(), 'hello-webhook', 'task.js');
  const original = fs.readFileSync(taskJsPath, 'utf8');

  await withGuaranteedCleanup(
    async () => {
      fs.writeFileSync(taskJsPath, original + '\n// e2e-pending-signal-probe\n', 'utf8');
      await waitForTaskCondition(request, WEBHOOK_TASK_ID, (t) => t.pending_approval === true);
      await body();
    },
    async () => {
      fs.writeFileSync(taskJsPath, original, 'utf8');
      await waitForTaskCondition(request, WEBHOOK_TASK_ID, (t) => t.pending_approval !== true);
    },
  );
}

test.describe('withGuaranteedCleanup', () => {
  test('surfaces a cleanup failure when body succeeded', async () => {
    const cleanupErr = new Error('cleanup boom');
    await expect(
      withGuaranteedCleanup(
        async () => {},
        async () => { throw cleanupErr; },
      ),
    ).rejects.toBe(cleanupErr);
  });

  test('a body failure wins over a cleanup failure, not swallowed', async () => {
    const bodyErr = new Error('body boom');
    const cleanupErr = new Error('cleanup boom');
    await expect(
      withGuaranteedCleanup(
        async () => { throw bodyErr; },
        async () => { throw cleanupErr; },
      ),
    ).rejects.toBe(bodyErr);
  });

  test('cleanup always runs even when body succeeds', async () => {
    let cleanupRan = false;
    await withGuaranteedCleanup(
      async () => {},
      async () => { cleanupRan = true; },
    );
    expect(cleanupRan).toBe(true);
  });

  test('a falsy body throw still counts as a failure, not swallowed by a successful cleanup', async () => {
    await expect(
      withGuaranteedCleanup(
        async () => { throw undefined; },
        async () => {},
      ),
    ).rejects.toBeUndefined();
  });

  test('a cleanup failure alongside a body failure is logged, not silently dropped', async () => {
    const bodyErr = new Error('body boom');
    const cleanupErr = new Error('cleanup boom');
    const originalConsoleError = console.error;
    const logged: unknown[][] = [];
    console.error = (...args: unknown[]) => { logged.push(args); };
    try {
      await expect(
        withGuaranteedCleanup(
          async () => { throw bodyErr; },
          async () => { throw cleanupErr; },
        ),
      ).rejects.toBe(bodyErr);
    } finally {
      console.error = originalConsoleError;
    }
    expect(logged.some((args) => args.includes(cleanupErr))).toBe(true);
  });
});

test.describe('Task list pending-approval signals', () => {
  test('a pending row reads as held, not live: no green dot, no dead link, Run disabled', async ({ page, request }) => {
    await withPendingWebhookChange(request, async () => {
      await gotoWebui(page);
      const row = page.locator(`dc-task-list tr[data-task-id="${WEBHOOK_TASK_ID}"]`);
      await expect(row).toBeVisible();

      // Toggle must not read as a plain healthy "on" green dot — distinct
      // held tint, tooltip explains triggers aren't armed.
      const toggle = row.locator('.toggle-btn');
      await expect(toggle).toHaveClass(/held/);
      await expect(toggle).not.toHaveClass(/\bon\b/);
      await expect(toggle).toHaveAttribute('title', /pending approval/);

      // The webhook route 404s until approved — must not render as a live,
      // clickable hyperlink identical to an armed webhook.
      const triggerCell = row.locator('td').nth(2);
      await expect(triggerCell.locator('a')).toHaveCount(0);
      await expect(triggerCell).toContainText('(proposed)');

      // Run must not silently no-op against the server's 400 — disabled
      // with a tooltip explaining why.
      const runBtn = row.locator('button', { hasText: 'Run' });
      await expect(runBtn).toBeDisabled();
      await expect(runBtn).toHaveAttribute('title', /pending approval/);
    });
  });

  test('a pending count/filter appears on the list and narrows it', async ({ page, request }) => {
    await withPendingWebhookChange(request, async () => {
      await gotoWebui(page);
      const rows = page.locator('dc-task-list tbody tr');
      const idsBefore = await rows.evaluateAll(els => els.map(el => el.getAttribute('data-task-id')));
      expect(idsBefore.length).toBeGreaterThan(1); // otherwise the filter narrowing this test checks is a no-op

      const chip = page.locator('dc-task-list button.pending-filter');
      await expect(chip).toBeVisible();
      await expect(chip).toContainText('pending approval');

      await chip.click();
      const row = page.locator(`dc-task-list tr[data-task-id="${WEBHOOK_TASK_ID}"]`);
      await expect(row).toBeVisible();
      await expect(row.locator('.badge-pending-approval')).toBeVisible();

      // Every row left visible under the filter must itself be pending —
      // not just that the known pending row survived narrowing.
      const visibleCount = await rows.count();
      for (let i = 0; i < visibleCount; i++) {
        await expect(rows.nth(i).locator('.badge-pending-approval')).toBeVisible();
      }
      expect(visibleCount).toBeLessThan(idsBefore.length);

      // Toggling the filter back off restores the exact original row set.
      await chip.click();
      await expect.poll(() => rows.evaluateAll(els => els.map(el => el.getAttribute('data-task-id'))))
        .toEqual(idsBefore);
    });
  });

  test('the notification tray surfaces the pending transition', async ({ page, request }) => {
    await gotoWebui(page);
    await page.waitForSelector('dc-notif-panel', { timeout: 15_000, state: 'attached' });

    await withPendingWebhookChange(request, async () => {
      const bell = page.locator('dc-notif-panel [title="Notifications"]');
      await expect(bell).toBeVisible();
      await bell.click();

      const entry = page.locator('dc-notif-panel').locator(`text=${WEBHOOK_TASK_ID}`);
      await expect(entry).toBeVisible({ timeout: 15_000 });
      const reviewLink = page.locator('dc-notif-panel a', { hasText: 'Review' });
      await expect(reviewLink).toBeVisible();
    });
  });

  // Reproduces the bug directly: any /run failure — not just the
  // pending-approval case — used to surface as a browser alert() with raw
  // JSON. Force a failure on a healthy task's Run and confirm it now
  // renders as a dismissible toast with no native dialog at all.
  test('a Run failure surfaces via toast, never a native alert', async ({ page }) => {
    await gotoWebui(page);
    let dialogFired = false;
    page.on('dialog', () => { dialogFired = true; });

    await page.route('**/api/tasks/*/run', (route) => route.fulfill({
      status: 500,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'simulated failure' }),
    }));

    // Any pending-approval task elsewhere in the shared fixture set has its
    // Run button natively disabled by this same PR's fix, and a disabled
    // button never dispatches a click at all (force:true only bypasses
    // Playwright's own actionability checks, not the browser's native
    // disabled semantics) — so scope to a button that isn't disabled rather
    // than assuming the first "Run" in DOM order is clickable.
    const runBtn = page.locator('dc-task-list button:not([disabled])', { hasText: 'Run' }).first();
    await runBtn.click();

    const toast = page.locator('dc-toast .toast');
    await expect(toast).toBeVisible({ timeout: 5_000 });
    await expect(toast).toContainText('Failed to run task');
    expect(dialogFired).toBe(false);
  });
});
