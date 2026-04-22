/**
 * webui-task.spec.ts
 *
 * E2E tests for the dicode web dashboard served as a webhook task at /hooks/webui.
 *
 * Covers:
 * - Unauthenticated access triggers auth overlay (or redirect to login)
 * - Dashboard loads at /hooks/webui after authentication
 * - Task list (dc-task-list) is visible
 * - Navigation between task list, task detail, and run detail works
 *
 * Adapted from the worktree e2e specs (task-list, task-detail, run-detail).
 * Key difference: SPA is now at /hooks/webui instead of /app/.
 */

import { test, expect } from '@playwright/test';

const WEBUI_URL = '/hooks/webui';

/** Navigate to the webui and wait for the auth overlay or app to appear. */
async function gotoWebui(page: import('@playwright/test').Page) {
  await page.goto(WEBUI_URL);
}

/** Submit the auth overlay if it is present. Requires DICODE_PASS env var. */
async function loginIfPrompted(page: import('@playwright/test').Page) {
  const pass = process.env.DICODE_PASS || '';
  // Check if the auth overlay or a password input is visible
  const overlay = page.locator('dc-auth-overlay');
  const pwInput = page.locator('#auth-pw');
  try {
    await pwInput.waitFor({ state: 'visible', timeout: 5_000 });
    await pwInput.fill(pass);
    await page.locator('dc-auth-overlay button', { hasText: 'Sign in' }).click();
    await pwInput.waitFor({ state: 'hidden', timeout: 10_000 });
  } catch (_) {
    // Already logged in or no overlay present
  }
}

/** Wait for the task list component to finish loading. */
async function waitForTaskList(page: import('@playwright/test').Page) {
  await page.waitForSelector('dc-task-list', { timeout: 10_000 });
  await page.waitForFunction(() => {
    const el = document.querySelector('dc-task-list');
    if (!el) return false;
    return !el.textContent?.includes('Loading');
  }, { timeout: 15_000 });
}

// ── Task List ──────────────────────────────────────────────────────────────────

test.describe('WebUI dashboard — Task List', () => {
  test.beforeEach(async ({ page }) => {
    await gotoWebui(page);
    await loginIfPrompted(page);
    await waitForTaskList(page);
  });

  test('dashboard loads at /hooks/webui', async ({ page }) => {
    expect(page.url()).toContain('/hooks/webui');
    await expect(page.locator('dc-task-list')).toBeVisible();
  });

  test('renders Tasks heading', async ({ page }) => {
    await expect(page.locator('h1', { hasText: 'Tasks' })).toBeVisible();
  });

  test('task list has header columns', async ({ page }) => {
    const thead = page.locator('thead').first();
    await expect(thead.locator('th', { hasText: 'ID' })).toBeVisible();
    await expect(thead.locator('th', { hasText: 'Name' })).toBeVisible();
    await expect(thead.locator('th', { hasText: 'Trigger' })).toBeVisible();
    await expect(thead.locator('th', { hasText: 'Last Run' })).toBeVisible();
    await expect(thead.locator('th', { hasText: 'Status' })).toBeVisible();
  });

  test('shows tasks from registered task sets', async ({ page }) => {
    // At least one task row should appear
    const rows = page.locator('tbody tr');
    await expect(rows.first()).toBeVisible();
  });

  test('tasks are grouped by namespace when namespaces exist', async ({ page }) => {
    // Namespace labels appear as coloured spans above their tables
    const nsLabels = page.locator('span[style*="text-transform:uppercase"]');
    const count = await nsLabels.count();
    // If any namespaced tasks are registered there will be at least one label
    if (count > 0) {
      await expect(nsLabels.first()).toBeVisible();
    }
  });

  test('clicking a task row navigates to task detail', async ({ page }) => {
    // dc-task-list re-renders on every WS run:* event (cron fires every
    // minute in the fixtures) so the <a> can be "not stable" for Playwright's
    // actionability check. force:true bypasses that — the @click handler
    // does its own pushState via navigate().
    const taskLink = page.locator('td a').first();
    await taskLink.click({ force: true });

    await page.waitForFunction(
      (prefix) => location.pathname.startsWith(prefix),
      WEBUI_URL + '/tasks/',
      { timeout: 10_000 },
    );
    await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
  });
});

// ── Task Detail ───────────────────────────────────────────────────────────────

test.describe('WebUI dashboard — Task Detail', () => {
  let firstTaskID: string;

  test.beforeAll(async ({ browser }) => {
    // Discover the first available task ID via API.
    const ctx = await browser.newContext();
    const req = await ctx.request.get('/api/tasks');
    if (req.ok()) {
      const tasks = await req.json();
      if (Array.isArray(tasks) && tasks.length > 0) {
        firstTaskID = tasks[0].id;
      }
    }
    await ctx.close();
  });

  test('task detail page shows task name', async ({ page }) => {
    if (!firstTaskID) test.skip();
    await gotoWebui(page);
    await loginIfPrompted(page);
    // Navigate client-side to the task detail
    await page.waitForSelector('dc-task-list', { timeout: 10_000 });
    await page.evaluate((id) => window.navigate('/tasks/' + id), firstTaskID);
    await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-task-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });
    await expect(page.locator('h1').first()).toBeVisible();
  });

  test('task detail shows Run now button', async ({ page }) => {
    if (!firstTaskID) test.skip();
    await gotoWebui(page);
    await loginIfPrompted(page);
    await page.waitForSelector('dc-task-list', { timeout: 10_000 });
    await page.evaluate((id) => window.navigate('/tasks/' + id), firstTaskID);
    await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-task-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });
    await expect(page.locator('button', { hasText: 'Run now' })).toBeVisible();
  });

  test('task detail shows recent runs section', async ({ page }) => {
    if (!firstTaskID) test.skip();
    await gotoWebui(page);
    await loginIfPrompted(page);
    await page.waitForSelector('dc-task-list', { timeout: 10_000 });
    await page.evaluate((id) => window.navigate('/tasks/' + id), firstTaskID);
    await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-task-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });
    await expect(page.locator('h2', { hasText: 'Recent runs' })).toBeVisible();
  });
});

// ── Run Detail ────────────────────────────────────────────────────────────────

test.describe('WebUI dashboard — Run Detail', () => {
  let firstTaskID: string;

  test.beforeAll(async ({ browser }) => {
    const ctx = await browser.newContext();
    const req = await ctx.request.get('/api/tasks');
    if (req.ok()) {
      const tasks = await req.json();
      // Prefer a manual task (no trigger) for predictable run behaviour
      const manual = tasks.find((t: { trigger_label?: string }) => t.trigger_label === 'manual');
      firstTaskID = (manual || tasks[0])?.id;
    }
    await ctx.close();
  });

  test('triggering a run navigates to run detail', async ({ page }) => {
    if (!firstTaskID) test.skip();
    await gotoWebui(page);
    await loginIfPrompted(page);
    await page.waitForSelector('dc-task-list', { timeout: 10_000 });
    await page.evaluate((id) => window.navigate('/tasks/' + id), firstTaskID);
    await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-task-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });

    await page.locator('button', { hasText: 'Run now' }).click({ force: true });

    // Should navigate to /hooks/webui/runs/{runID} client-side.
    // waitForURL is more robust than waitForFunction against Lit re-renders.
    await page.waitForURL(/\/hooks\/webui\/runs\//, { timeout: 15_000 });
    await page.waitForSelector('dc-run-detail', { timeout: 10_000 });
    await expect(page.locator('.badge').first()).toBeVisible();
  });

  test('run detail page shows Logs heading', async ({ page, request }) => {
    if (!firstTaskID) test.skip();
    // Fire a run via API
    const res = await request.post(`/api/tasks/${encodeURIComponent(firstTaskID)}/run`);
    if (!res.ok()) test.skip();
    const { runId } = await res.json();

    await gotoWebui(page);
    await loginIfPrompted(page);
    await page.waitForSelector('dc-task-list', { timeout: 10_000 });
    await page.evaluate((id) => window.navigate('/runs/' + id), runId);
    await page.waitForSelector('dc-run-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-run-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });

    await expect(page.locator('h2', { hasText: 'Logs' })).toBeVisible();
    await expect(page.locator('#log-output')).toBeVisible();
  });
});

// ── Navigation ────────────────────────────────────────────────────────────────

test.describe('WebUI dashboard — Navigation', () => {
  test.beforeEach(async ({ page }) => {
    await gotoWebui(page);
    await loginIfPrompted(page);
    await waitForTaskList(page);
  });

  test('nav link to Sources navigates client-side', async ({ page }) => {
    await page.locator('nav a', { hasText: 'Sources' }).click({ force: true });
    await page.waitForFunction(
      (base) => location.pathname.startsWith(base + '/sources'),
      WEBUI_URL,
      { timeout: 5_000 },
    );
    await page.waitForSelector('dc-sources', { timeout: 10_000 });
  });

  test('nav link to Config navigates client-side', async ({ page }) => {
    await page.locator('nav a', { hasText: 'Config' }).click({ force: true });
    await page.waitForFunction(
      (base) => location.pathname.startsWith(base + '/config'),
      WEBUI_URL,
      { timeout: 5_000 },
    );
    await page.waitForSelector('dc-config', { timeout: 10_000 });
  });

  test('nav link Tasks returns to task list', async ({ page }) => {
    await page.locator('nav a', { hasText: 'Config' }).click({ force: true });
    await page.waitForSelector('dc-config', { timeout: 10_000 });
    await page.locator('nav a', { hasText: 'Tasks' }).first().click({ force: true });
    await page.waitForSelector('dc-task-list', { timeout: 10_000 });
  });
});
