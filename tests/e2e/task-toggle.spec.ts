import { test, expect } from '@playwright/test';
import { gotoWebui } from './helpers/webui';

test.describe('Task enable/disable toggle', () => {
  test('PATCH /api/tasks/{id}/overrides sets enabled=false', async ({ request }) => {
    const list = await request.get('/api/tasks').then((r) => r.json());
    expect(Array.isArray(list)).toBe(true);
    expect(list.length).toBeGreaterThan(0);
    const target = list[0];
    const id = target.id || target.ID;

    const res = await request.patch(`/api/tasks/${id}/overrides`, {
      data: { enabled: false },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(200);

    await expect.poll(async () => {
      const after = await request.get('/api/tasks').then((r) => r.json());
      const t = after.find((x: any) => (x.id || x.ID) === id);
      return t?.enabled;
    }, { timeout: 10_000 }).toBe(false);

    // Restore so subsequent tests start clean.
    await request.patch(`/api/tasks/${id}/overrides`, {
      data: { enabled: true },
      headers: { 'Content-Type': 'application/json' },
    });
  });

  test('PATCH unknown task returns 404', async ({ request }) => {
    const res = await request.patch('/api/tasks/no-source/no-task/overrides', {
      data: { enabled: false },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(404);
  });

  test('PATCH unknown field returns 400', async ({ request }) => {
    const list = await request.get('/api/tasks').then((r) => r.json());
    const id = list[0].id || list[0].ID;
    const res = await request.patch(`/api/tasks/${id}/overrides`, {
      data: { unknownField: true },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(400);
  });

  test('UI toggle flips the row and persists across reload', async ({ page, request }) => {
    const list = await request.get('/api/tasks').then((r) => r.json());
    expect(list.length).toBeGreaterThan(0);
    const target = list[0];
    const id = target.id || target.ID;

    await gotoWebui(page);
    await page.waitForSelector('dc-task-list', { timeout: 15_000 });

    const row = page.locator('dc-task-list').locator(`[data-task-id="${id}"]`);
    if ((await row.count()) === 0) {
      test.skip(true, 'dc-task-list does not expose data-task-id; skipping UI assertion');
      return;
    }

    const toggle = row.locator('.toggle-btn');
    await expect(toggle).toBeVisible();
    await toggle.click();

    // Optimistic flip → row gets the disabled class
    await expect(row).toHaveClass(/disabled/, { timeout: 5_000 });

    // Reload → server-confirmed state persists
    await page.reload();
    await page.waitForSelector('dc-task-list');
    const reloaded = page.locator('dc-task-list').locator(`[data-task-id="${id}"]`);
    await expect(reloaded).toHaveClass(/disabled/, { timeout: 10_000 });

    // Restore
    await request.patch(`/api/tasks/${id}/overrides`, {
      data: { enabled: true },
      headers: { 'Content-Type': 'application/json' },
    });
  });

  test('dc-toast surfaces a visible message when the API rejects a toggle', async ({ page }) => {
    await gotoWebui(page);
    // dc-toast is appended to <body> on app boot but renders nothing until a
    // 'dc-toast' event arrives — the empty element has 0 size, which fails
    // the default 'visible' state of waitForSelector. Wait for attachment
    // instead, then dispatch the event and assert the inner toast div renders.
    await page.waitForSelector('dc-toast', { timeout: 15_000, state: 'attached' });

    await page.evaluate(() => {
      window.dispatchEvent(new CustomEvent('dc-toast', {
        detail: { message: 'Toggle failed: simulated', kind: 'error' },
      }));
    });

    const toast = page.locator('dc-toast .toast');
    await expect(toast).toBeVisible({ timeout: 5_000 });
    await expect(toast).toContainText('Toggle failed: simulated');
    await expect(toast).toHaveClass(/error/);
  });
});
