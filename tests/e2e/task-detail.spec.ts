/**
 * task-detail.spec.ts
 *
 * Tests for the task detail page (dc-task-detail component):
 * - Shows task name, description, trigger label
 * - Manual trigger button present
 * - Cron tasks show trigger label
 * - "Run now" button fires a run and navigates to run detail
 */

import { test, expect } from '@playwright/test';

const MANUAL_TASK_ID = 'e2e-tests/hello-manual';
const CRON_TASK_ID = 'e2e-tests/hello-cron';
const WEBHOOK_TASK_ID = 'e2e-tests/hello-webhook';

/** Navigate to task detail and wait for it to load */
async function openTask(page: import('@playwright/test').Page, taskID: string) {
  await page.goto(`/tasks/${encodeURIComponent(taskID)}`);
  await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
  await page.waitForFunction(() => {
    const el = document.querySelector('dc-task-detail');
    return el && !el.textContent?.includes('Loading');
  }, { timeout: 15_000 });
}

test.describe('Task Detail', () => {
  test('shows task name for manual task', async ({ page }) => {
    await openTask(page, MANUAL_TASK_ID);
    await expect(page.locator('h1', { hasText: 'Hello Manual' })).toBeVisible();
  });

  test('shows task description', async ({ page }) => {
    await openTask(page, MANUAL_TASK_ID);
    await expect(page.locator('text=A simple manual task for e2e testing')).toBeVisible();
  });

  test('shows trigger label for manual task', async ({ page }) => {
    await openTask(page, MANUAL_TASK_ID);
    // The trigger card shows "Trigger: manual"
    await expect(page.locator('.card', { hasText: 'Trigger:' })).toContainText('manual');
  });

  test('Run now button is present', async ({ page }) => {
    await openTask(page, MANUAL_TASK_ID);
    await expect(page.locator('button', { hasText: 'Run now' })).toBeVisible();
  });

  test('cron task shows cron trigger label', async ({ page }) => {
    await openTask(page, CRON_TASK_ID);
    await expect(page.locator('h1', { hasText: 'Hello Cron' })).toBeVisible();
    const triggerCard = page.locator('.card', { hasText: 'Trigger:' });
    // Should contain some cron-related text — at minimum not "manual"
    const text = await triggerCard.textContent();
    expect(text).toMatch(/cron|every/i);
  });

  test('webhook task shows webhook path in trigger', async ({ page }) => {
    await openTask(page, WEBHOOK_TASK_ID);
    await expect(page.locator('h1', { hasText: 'Hello Webhook' })).toBeVisible();
    const triggerCard = page.locator('.card', { hasText: 'Trigger:' });
    await expect(triggerCard).toContainText('webhook');
  });

  test('recent runs table is present', async ({ page }) => {
    await openTask(page, MANUAL_TASK_ID);
    await expect(page.locator('h2', { hasText: 'Recent runs' })).toBeVisible();
  });

  test('trigger button fires run and navigates to run detail', async ({ page }) => {
    await openTask(page, MANUAL_TASK_ID);
    const runBtn = page.locator('button', { hasText: 'Run now' });
    await runBtn.click();

    // Should navigate to /runs/{runID}
    await page.waitForURL(/\/runs\//, { timeout: 15_000 });
    await page.waitForSelector('dc-run-detail', { timeout: 10_000 });

    // Run detail page shows the status badge
    await expect(page.locator('.badge')).toBeVisible();
  });

  test('API returns correct task shape', async ({ request }) => {
    const res = await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`);
    expect(res.ok()).toBe(true);
    const body = await res.json();
    expect(body).toMatchObject({
      id: MANUAL_TASK_ID,
      name: 'Hello Manual',
      trigger_label: 'manual',
      script_file: 'task.js',
    });
  });
});
