/**
 * config.spec.ts
 *
 * Tests for config read/write via the REST API and UI.
 *
 * Runs in the unauthenticated project where server.auth=false. The require-auth
 * middleware is therefore a no-op and protected endpoints (/api/config/raw,
 * POST /api/config/raw) are reachable without a cookie. Auth-mode coverage of
 * those endpoints lives in auth.spec.ts.
 */

import { test, expect } from '@playwright/test';
import { gotoWebui, navigateInSpa } from './helpers/webui';

test.describe('Config API', () => {
  test('GET /api/config returns config object with our test port', async ({ request }) => {
    const res = await request.get('/api/config');
    expect(res.ok()).toBe(true);
    const cfg = await res.json() as Record<string, unknown>;
    const server = (cfg.Server || cfg.server) as Record<string, unknown>;
    expect(server).toBeTruthy();
    expect(server.Port || server.port).toBe(8765);
  });

  test('GET /api/config does not leak secret field', async ({ request }) => {
    const res = await request.get('/api/config');
    expect(res.ok()).toBe(true);
    const cfg = await res.json() as Record<string, unknown>;
    const server = (cfg.Server || cfg.server) as Record<string, unknown>;
    // server.secret carries `json:"-"` — must not appear under any casing.
    expect(server).not.toHaveProperty('Secret');
    expect(server).not.toHaveProperty('secret');
  });

  test('GET /api/config returns sources array including e2e-tests', async ({ request }) => {
    const res = await request.get('/api/config');
    expect(res.ok()).toBe(true);
    const cfg = await res.json() as Record<string, unknown>;
    const sources = (cfg.Sources || cfg.sources) as Array<Record<string, unknown>>;
    expect(Array.isArray(sources)).toBe(true);
    const e2e = sources.find((s) => (s.Name || s.name) === 'e2e-tests');
    expect(e2e).toBeTruthy();
    expect(e2e!.Type || e2e!.type).toBe('local');
  });

  test('GET /api/config/raw returns YAML content', async ({ request }) => {
    const res = await request.get('/api/config/raw');
    expect(res.ok()).toBe(true);
    const body = await res.json() as { content: string };
    expect(typeof body.content).toBe('string');
    expect(body.content).toContain('8765');
  });

  test('POST /api/config/raw rejects invalid YAML with 400', async ({ request }) => {
    const res = await request.post('/api/config/raw', {
      data: { content: 'this: is: invalid: yaml: {' },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(400);
  });

  test('POST /api/config/raw persists valid YAML and round-trips a marker', async ({ request }) => {
    const rawRes = await request.get('/api/config/raw');
    expect(rawRes.ok()).toBe(true);
    const { content: original } = await rawRes.json() as { content: string };

    const updated = original + '\n# e2e-config-test-marker\n';
    const saveRes = await request.post('/api/config/raw', {
      data: { content: updated },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(saveRes.ok()).toBe(true);

    const readBackRes = await request.get('/api/config/raw');
    const { content: readBack } = await readBackRes.json() as { content: string };
    expect(readBack).toContain('e2e-config-test-marker');

    // Restore.
    await request.post('/api/config/raw', {
      data: { content: original },
      headers: { 'Content-Type': 'application/json' },
    });
  });
});

test.describe('Config UI', () => {
  test('navigating to /config shows config page', async ({ page }) => {
    await gotoWebui(page);
    await navigateInSpa(page, '/config');
    await page.waitForSelector('dc-config', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-config');
      return !!el && !el.textContent?.includes('Loading') && el.textContent !== '';
    }, { timeout: 15_000 });

    await expect(page).toHaveURL(/\/config/);
  });

  test('config page contains server settings section', async ({ page }) => {
    await gotoWebui(page);
    await navigateInSpa(page, '/config');
    await page.waitForSelector('dc-config', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-config');
      return !!el && (el.textContent ?? '').trim().length > 50;
    }, { timeout: 15_000 });

    const text = await page.locator('dc-config').textContent();
    expect(text).toMatch(/server|Server|config/i);
  });

  test('header nav link navigates to config page', async ({ page }) => {
    await gotoWebui(page);
    // force:true — dc-task-list re-renders on WS events and can desync the
    // stability check even though the <a> in <header> is in static DOM.
    await page.locator('header nav a', { hasText: 'Config' }).click({ force: true });
    await page.waitForFunction(
      (base) => location.pathname.startsWith(base + '/config'),
      '/hooks/webui',
      { timeout: 5_000 },
    );
    await expect(page.locator('dc-config')).toBeVisible({ timeout: 10_000 });
  });
});
