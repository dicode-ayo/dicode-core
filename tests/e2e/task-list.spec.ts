/**
 * task-list.spec.ts
 *
 * Tests for the task list page (dc-task-list component):
 * - Loads the page and shows tasks from the test TaskSet
 * - Tasks are grouped by namespace (e2e-tests)
 * - Each row shows ID, name, trigger type
 * - Clicking a task row navigates to task detail
 */

import { test, expect } from '@playwright/test';

test.describe('Task List', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
    // Wait for the Lit component to render (light DOM, so just wait for table)
    await page.waitForSelector('dc-task-list', { timeout: 10_000 });
    // Wait until tasks are loaded — the Loading... text disappears
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-task-list');
      if (!el) return false;
      return !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });
  });

  test('renders page title', async ({ page }) => {
    await expect(page.locator('h1', { hasText: 'Tasks' })).toBeVisible();
  });

  test('shows tasks registered from test TaskSet', async ({ page }) => {
    // Tasks have IDs like "e2e-tests/hello-manual", "e2e-tests/hello-cron", etc.
    await expect(page.locator('td a', { hasText: 'hello-manual' })).toBeVisible();
    await expect(page.locator('td a', { hasText: 'hello-cron' })).toBeVisible();
    await expect(page.locator('td a', { hasText: 'hello-webhook' })).toBeVisible();
  });

  test('tasks are grouped by namespace', async ({ page }) => {
    // The namespace label should appear as a header above the grouped table
    await expect(page.locator('text=e2e-tests')).toBeVisible();
  });

  test('task row shows name, trigger type', async ({ page }) => {
    // hello-manual has no trigger — should show "manual"
    const manualRow = page.locator('tr', { hasText: 'hello-manual' });
    await expect(manualRow.locator('.meta', { hasText: 'manual' })).toBeVisible();

    // hello-cron should show its cron expression or "cron" label
    const cronRow = page.locator('tr', { hasText: 'hello-cron' });
    await expect(cronRow.locator('.meta')).toBeVisible();
  });

  test('clicking a task navigates to task detail', async ({ page }) => {
    // Click the first task link in the table
    const taskLink = page.locator('td a', { hasText: 'hello-manual' }).first();
    await taskLink.click();

    // Should navigate to /tasks/... route
    await page.waitForURL(/\/tasks\//);
    await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
    await expect(page.locator('h1', { hasText: 'Hello Manual' })).toBeVisible();
  });

  test('task list has header columns', async ({ page }) => {
    const thead = page.locator('thead').first();
    await expect(thead.locator('th', { hasText: 'ID' })).toBeVisible();
    await expect(thead.locator('th', { hasText: 'Name' })).toBeVisible();
    await expect(thead.locator('th', { hasText: 'Trigger' })).toBeVisible();
    await expect(thead.locator('th', { hasText: 'Last Run' })).toBeVisible();
    await expect(thead.locator('th', { hasText: 'Status' })).toBeVisible();
  });
});
