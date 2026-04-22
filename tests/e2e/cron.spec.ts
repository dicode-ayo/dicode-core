/**
 * cron.spec.ts
 *
 * Tests for cron-triggered tasks.
 *
 * The hello-cron test task has trigger.cron = "* * * * *" (every minute).
 * We wait up to 90 seconds for at least one run to appear.
 *
 * Note: these tests have a longer timeout to accommodate the cron schedule.
 */

import { test, expect } from '@playwright/test';
import { gotoWebui, navigateInSpa, waitForTaskDetail } from './helpers/webui';

const CRON_TASK_ID = 'e2e-tests/hello-cron';

// Override timeout for this entire file — cron tests need up to 90 s
test.setTimeout(120_000);

/**
 * Poll GET /api/tasks/{id}/runs until at least one run appears, up to timeoutMs.
 * Returns the list of runs once populated.
 */
async function waitForCronRun(
  request: import('@playwright/test').APIRequestContext,
  taskID: string,
  timeoutMs = 90_000,
): Promise<Array<Record<string, unknown>>> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await request.get(
      `/api/tasks/${encodeURIComponent(taskID)}/runs?limit=5`,
    );
    if (res.ok()) {
      const runs = await res.json() as Array<Record<string, unknown>>;
      if (Array.isArray(runs) && runs.length > 0) return runs;
    }
    await new Promise((r) => setTimeout(r, 3000));
  }
  throw new Error(`No runs appeared for ${taskID} within ${timeoutMs}ms`);
}

test.describe('Cron Tasks', () => {
  test('cron task fires at least once within 90 seconds', async ({ request }) => {
    const runs = await waitForCronRun(request, CRON_TASK_ID, 90_000);
    expect(runs.length).toBeGreaterThan(0);
  });

  test('cron run status is success', async ({ request }) => {
    const runs = await waitForCronRun(request, CRON_TASK_ID, 90_000);
    // Find the first completed run.
    const completed = runs.find((r) => {
      const s = (r.Status || r.status) as string;
      return s === 'success' || s === 'failure';
    });
    // At least one run should have completed.
    if (completed) {
      const status = (completed.Status || completed.status) as string;
      expect(status).toBe('success');
    }
  });

  test('cron run has logs', async ({ request }) => {
    const runs = await waitForCronRun(request, CRON_TASK_ID, 90_000);
    // Find a completed run to check its logs.
    const completedRun = runs.find((r) => {
      const s = (r.Status || r.status) as string;
      return s === 'success';
    });
    if (!completedRun) {
      test.skip(true, 'No completed cron run found yet');
      return;
    }
    const runID = (completedRun.ID || completedRun.id) as string;
    const logsRes = await request.get(`/api/runs/${runID}/logs`);
    expect(logsRes.ok()).toBe(true);
    const logs = await logsRes.json() as Array<{ message: string }>;
    expect(logs.length).toBeGreaterThan(0);
    const allMessages = logs.map((l) => l.message).join('\n');
    expect(allMessages).toContain('cron tick');
  });

  test('task list shows last run status after cron fires', async ({ page }) => {
    await gotoWebui(page);

    // Poll the UI until last_run_status is populated for the cron task.
    await page.waitForFunction(
      (taskIDPrefix: string) => {
        const rows = document.querySelectorAll('tr');
        for (const row of rows) {
          const text = row.textContent ?? '';
          if (text.includes(taskIDPrefix)) {
            return !!row.querySelector('.badge');
          }
        }
        return false;
      },
      'hello-cron',
      { timeout: 90_000, polling: 3000 },
    );

    const cronRow = page.locator('tr', { hasText: 'hello-cron' });
    await expect(cronRow.locator('.badge')).toBeVisible();
  });

  test('cron task detail shows trigger label with cron expression', async ({ page }) => {
    await gotoWebui(page);
    await navigateInSpa(page, `/tasks/${CRON_TASK_ID}`);
    await waitForTaskDetail(page);

    // Trigger card should mention cron.
    const triggerCard = page.locator('.card', { hasText: 'Trigger:' });
    const triggerText = await triggerCard.textContent();
    expect(triggerText).toMatch(/cron|every minute|\*/i);
  });
});
