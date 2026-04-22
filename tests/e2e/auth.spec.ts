/**
 * auth.spec.ts
 *
 * Authentication tests — runs against the 'authenticated' project which starts
 * dicode with server.auth: true and server.secret: test-passphrase-12345.
 *
 * Covers the redirect-to-/login flow added in dicode-core#96/#131:
 * - Unauthenticated API calls → 401
 * - Wrong passphrase → 401
 * - Correct passphrase → 200 + session cookie
 * - Authenticated calls succeed
 * - Browser GET /hooks/webui without session → 303 → /login?next=/hooks/webui
 * - /login renders an HTML form
 * - After login the SPA at /hooks/webui loads
 * - Webhooks bypass the auth wall
 * - /api/auth/passphrase reports the source
 * - /api/auth/logout invalidates the session
 */

import { test, expect } from '@playwright/test';
import { TEST_PASSPHRASE, login } from './helpers/auth';

test.describe('Authentication', () => {
  test('unauthenticated GET /api/tasks → 401', async ({ request }) => {
    const res = await request.get('/api/tasks');
    expect(res.status()).toBe(401);
  });

  test('unauthenticated GET /api/runs/{id} → 401', async ({ request }) => {
    const res = await request.get('/api/runs/nonexistent-run-id');
    expect(res.status()).toBe(401);
  });

  test('POST /api/auth/login with wrong passphrase → 401', async ({ request }) => {
    const res = await request.post('/api/auth/login', {
      data: { password: 'completely-wrong-passphrase' },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(401);
  });

  test('POST /api/auth/login with correct passphrase → 200', async ({ request }) => {
    const res = await request.post('/api/auth/login', {
      data: { password: TEST_PASSPHRASE },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as { status: string };
    expect(body.status).toBe('ok');
  });

  test('session cookie is set after successful login', async ({ request }) => {
    const res = await request.post('/api/auth/login', {
      data: { password: TEST_PASSPHRASE },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.ok()).toBe(true);
    const setCookie = res.headers()['set-cookie'] ?? '';
    expect(setCookie).toContain('dicode_secrets_sess=');
  });

  test('authenticated request to /api/tasks succeeds', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.get('/api/tasks');
    expect(res.ok()).toBe(true);
    const tasks = await res.json() as unknown[];
    expect(Array.isArray(tasks)).toBe(true);
  });

  test('webhooks bypass auth wall (no session required)', async ({ request }) => {
    const res = await request.post('/hooks/test-webhook', {
      headers: { 'Content-Type': 'application/json' },
      data: { auth_test: true },
    });
    expect(res.status()).not.toBe(401);
    expect(res.status()).not.toBe(403);
  });

  test('UI: GET /hooks/webui without session redirects to /login with next', async ({ request }) => {
    // Browser-style GET (Accept: text/html) — webhookAuthGuard sends 303 → /login?next=...
    // Use maxRedirects: 0 to inspect the redirect itself.
    const res = await request.get('/hooks/webui', {
      headers: { Accept: 'text/html' },
      maxRedirects: 0,
    });
    expect(res.status()).toBe(303);
    const loc = res.headers()['location'] ?? '';
    expect(loc).toMatch(/^\/login\?next=/);
    expect(decodeURIComponent(loc)).toContain('/hooks/webui');
  });

  test('UI: /login renders an HTML form with a password input', async ({ page }) => {
    await page.goto('/login');
    await expect(page.locator('input[type=password], input[name=password]').first()).toBeVisible();
  });

  test('UI: submitting /login form with wrong passphrase shows error', async ({ page }) => {
    await page.goto('/login');
    await page.fill('input[name=password]', 'completely-wrong-passphrase');
    await page.locator('form button[type=submit], form input[type=submit]').first().click({ force: true });
    await expect(page.locator('body')).toContainText(/[Ii]ncorrect|[Ii]nvalid|[Ww]rong/);
  });

  test('UI: submitting /login form with correct passphrase loads SPA', async ({ page }) => {
    await page.goto('/login?next=' + encodeURIComponent('/hooks/webui'));
    await page.fill('input[name=password]', TEST_PASSPHRASE);
    await page.locator('form button[type=submit], form input[type=submit]').first().click({ force: true });

    // Form post → 303 → /hooks/webui → SPA loads.
    await page.waitForURL(/\/hooks\/webui/, { timeout: 10_000 });
    await page.waitForSelector('dc-task-list', { timeout: 15_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-task-list');
      return !!el && !el.textContent?.includes('Loading');
    }, { timeout: 20_000 });
    await expect(page.locator('h1', { hasText: 'Tasks' })).toBeVisible();
  });

  test('GET /api/auth/passphrase status returns source after login', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.get('/api/auth/passphrase');
    expect(res.ok()).toBe(true);
    const body = await res.json() as { source: string };
    // server.secret comes from YAML in this fixture.
    expect(body.source).toBe('yaml');
  });

  test('POST /api/auth/logout invalidates session', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);

    const tasksRes = await request.get('/api/tasks');
    expect(tasksRes.ok()).toBe(true);

    const logoutRes = await request.post('/api/auth/logout');
    expect(logoutRes.ok()).toBe(true);

    const afterRes = await request.get('/api/tasks');
    expect(afterRes.status()).toBe(401);
  });
});
