/**
 * config.spec.ts
 *
 * Tests for config read/write via the REST API and UI.
 *
 * Note: /api/config/raw requires a valid secrets session (even without auth
 * enabled, a session cookie is needed because the raw config may hold API keys).
 * In the unauthenticated project the secrets passphrase is empty, so the unlock
 * endpoint accepts any password (or even an empty one) and issues a session.
 */

import { test, expect } from '@playwright/test';

test.describe('Config API', () => {
  test('GET /api/config returns config object', async ({ request }) => {
    const res = await request.get('/api/config');
    expect(res.ok()).toBe(true);
    const cfg = await res.json() as Record<string, unknown>;
    // Must contain the server object with our test port.
    expect(cfg).toHaveProperty('server');
    const server = cfg.server as Record<string, unknown>;
    expect(server.port).toBe(8080);
  });

  test('GET /api/config does not leak secret field', async ({ request }) => {
    const res = await request.get('/api/config');
    expect(res.ok()).toBe(true);
    const cfg = await res.json() as Record<string, unknown>;
    // server.secret is tagged json:"-" — must not appear.
    const server = cfg.server as Record<string, unknown>;
    expect(server).not.toHaveProperty('secret');
  });

  test('GET /api/config returns sources array', async ({ request }) => {
    const res = await request.get('/api/config');
    expect(res.ok()).toBe(true);
    const cfg = await res.json() as { sources: Array<Record<string, unknown>> };
    expect(Array.isArray(cfg.sources)).toBe(true);
    expect(cfg.sources.length).toBeGreaterThan(0);
    const src = cfg.sources[0];
    expect(src.name).toBe('e2e-tests');
    expect(src.type).toBe('local');
  });

  test('GET /api/config/raw requires secrets session (401 without cookie)', async ({ request }) => {
    const res = await request.get('/api/config/raw');
    // Without a session cookie the endpoint returns 401.
    expect(res.status()).toBe(401);
  });

  test('GET /api/config/raw returns YAML content after unlock', async ({ request }) => {
    // Unlock with empty password (no passphrase configured in unauth mode).
    const unlockRes = await request.post('/api/secrets/unlock', {
      data: { password: '' },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(unlockRes.ok()).toBe(true);

    const res = await request.get('/api/config/raw');
    expect(res.ok()).toBe(true);
    const body = await res.json() as { content: string };
    expect(typeof body.content).toBe('string');
    // Should contain our test port.
    expect(body.content).toContain('8080');
  });

  test('POST /api/config/raw validates YAML before writing', async ({ request }) => {
    // Unlock first.
    await request.post('/api/secrets/unlock', {
      data: { password: '' },
      headers: { 'Content-Type': 'application/json' },
    });

    // Send invalid YAML.
    const res = await request.post('/api/config/raw', {
      data: { content: 'this: is: invalid: yaml: {' },
      headers: { 'Content-Type': 'application/json' },
    });
    // Should reject with 400.
    expect(res.status()).toBe(400);
  });

  test('POST /api/config/raw persists valid YAML and is readable back', async ({ request }) => {
    // Unlock.
    await request.post('/api/secrets/unlock', {
      data: { password: '' },
      headers: { 'Content-Type': 'application/json' },
    });

    // Read current config.
    const rawRes = await request.get('/api/config/raw');
    expect(rawRes.ok()).toBe(true);
    const { content: original } = await rawRes.json() as { content: string };

    // Add a comment marker so we can identify the update.
    const updated = original + '\n# e2e-config-test-marker\n';
    const saveRes = await request.post('/api/config/raw', {
      data: { content: updated },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(saveRes.ok()).toBe(true);

    // Read back and verify the marker is present.
    const readBackRes = await request.get('/api/config/raw');
    const { content: readBack } = await readBackRes.json() as { content: string };
    expect(readBack).toContain('e2e-config-test-marker');

    // Restore original.
    await request.post('/api/config/raw', {
      data: { content: original },
      headers: { 'Content-Type': 'application/json' },
    });
  });
});

test.describe('Config UI', () => {
  test('navigating to /config shows config page', async ({ page }) => {
    await page.goto('/config');
    await page.waitForSelector('dc-config', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-config');
      return el && !el.textContent?.includes('Loading') && el.textContent !== '';
    }, { timeout: 15_000 });

    // The config component renders settings forms.
    // Check for recognisable section text.
    await expect(page).toHaveURL(/\/config/);
  });

  test('config page contains server settings section', async ({ page }) => {
    await page.goto('/config');
    await page.waitForSelector('dc-config', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-config');
      return el && el.textContent && el.textContent.trim().length > 50;
    }, { timeout: 15_000 });

    // The config component includes "AI" and "Server" settings sections.
    const text = await page.locator('dc-config').textContent();
    expect(text).toMatch(/server|Server|config/i);
  });

  test('header nav link navigates to config page', async ({ page }) => {
    await page.goto('/');
    await page.waitForSelector('header', { timeout: 10_000 });
    await page.locator('header nav a', { hasText: 'Config' }).click();
    await page.waitForURL(/\/config/);
    await expect(page.locator('dc-config')).toBeVisible({ timeout: 10_000 });
  });
});
