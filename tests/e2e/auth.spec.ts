/**
 * auth.spec.ts
 *
 * Authentication tests — runs against the 'authenticated' project which starts
 * dicode with server.auth: true and server.secret: test-passphrase-12345.
 *
 * Key auth behaviours verified:
 * - Unauthenticated API calls → 401
 * - Wrong passphrase → 401
 * - Correct passphrase → 200 + session cookie
 * - Authenticated calls succeed
 * - UI redirects to auth overlay when not logged in
 * - After login task list is accessible
 * - Webhooks bypass the auth wall (no session needed)
 * - Passphrase change endpoint exists and works
 */

import { test, expect } from '@playwright/test';
import { TEST_PASSPHRASE, login } from './helpers/auth';

const BASE_URL = 'http://localhost:8080';

test.describe('Authentication', () => {
  test('unauthenticated GET /api/tasks → 401', async ({ request }) => {
    const res = await request.get('/api/tasks');
    expect(res.status()).toBe(401);
  });

  test('unauthenticated GET /api/runs/{id} → 401', async ({ request }) => {
    const res = await request.get('/api/runs/nonexistent-run-id');
    expect(res.status()).toBe(401);
  });

  test('POST /api/secrets/unlock with wrong passphrase → 401', async ({ request }) => {
    const res = await request.post('/api/secrets/unlock', {
      data: { password: 'completely-wrong-passphrase' },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.status()).toBe(401);
  });

  test('POST /api/secrets/unlock with correct passphrase → 200', async ({ request }) => {
    const res = await request.post('/api/secrets/unlock', {
      data: { password: TEST_PASSPHRASE },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as { status: string };
    expect(body.status).toBe('ok');
  });

  test('session cookie is set after successful login', async ({ request }) => {
    const res = await request.post('/api/secrets/unlock', {
      data: { password: TEST_PASSPHRASE },
      headers: { 'Content-Type': 'application/json' },
    });
    expect(res.ok()).toBe(true);
    const setCookie = res.headers()['set-cookie'] ?? '';
    expect(setCookie).toContain('dicode_secrets_sess=');
  });

  test('authenticated request to /api/tasks succeeds', async ({ request }) => {
    // Login first.
    await login(request, TEST_PASSPHRASE);

    // Now the request context has the session cookie — subsequent calls succeed.
    const res = await request.get('/api/tasks');
    expect(res.ok()).toBe(true);
    const tasks = await res.json() as unknown[];
    expect(Array.isArray(tasks)).toBe(true);
  });

  test('webhooks bypass auth wall (no session required)', async ({ request }) => {
    // Do NOT login — issue a fresh request context without session.
    // The /hooks/* paths are always public.
    const res = await request.post('/hooks/test-webhook', {
      headers: { 'Content-Type': 'application/json' },
      data: { auth_test: true },
    });
    // Should not be 401 — webhooks are public.
    expect(res.status()).not.toBe(401);
    expect(res.status()).not.toBe(403);
  });

  test('static assets bypass auth wall', async ({ request }) => {
    // /app/* static files must be reachable without a session (needed to render login).
    const res = await request.get('/app/app.js');
    expect(res.status()).not.toBe(401);
    expect(res.status()).not.toBe(403);
  });

  test('UI: visiting / shows auth overlay when not logged in', async ({ page }) => {
    await page.goto('/');
    // The auth overlay should appear — it's a fixed overlay element.
    await page.waitForSelector('dc-auth-overlay', { timeout: 15_000 });
    // The overlay becomes visible when the API returns 401.
    await page.waitForFunction(() => {
      const overlay = document.querySelector('dc-auth-overlay');
      if (!overlay) return false;
      // _visible property drives display; check for the password input rendered.
      return !!overlay.querySelector('#auth-pw');
    }, { timeout: 15_000 });

    await expect(page.locator('#auth-pw')).toBeVisible({ timeout: 10_000 });
  });

  test('UI: incorrect passphrase shows error message', async ({ page }) => {
    await page.goto('/');
    await page.waitForSelector('#auth-pw', { timeout: 15_000 });

    await page.fill('#auth-pw', 'wrong-passphrase');
    await page.locator('dc-auth-overlay button', { hasText: 'Sign in' }).click();

    // An error message should appear.
    await page.waitForFunction(() => {
      const overlay = document.querySelector('dc-auth-overlay');
      return overlay?.textContent?.includes('Incorrect') || overlay?.textContent?.includes('incorrect');
    }, { timeout: 10_000 });
    await expect(page.locator('dc-auth-overlay')).toContainText(/[Ii]ncorrect/);
  });

  test('UI: correct passphrase dismisses overlay and shows task list', async ({ page }) => {
    await page.goto('/');
    await page.waitForSelector('#auth-pw', { timeout: 15_000 });

    await page.fill('#auth-pw', TEST_PASSPHRASE);
    await page.locator('dc-auth-overlay button', { hasText: 'Sign in' }).click();

    // Overlay should disappear and task list should render.
    await page.waitForFunction(() => {
      const overlay = document.querySelector('dc-auth-overlay');
      // Overlay hidden = no #auth-pw visible
      return !overlay?.querySelector('#auth-pw');
    }, { timeout: 10_000 });

    // Task list should be loading.
    await page.waitForSelector('dc-task-list', { timeout: 15_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-task-list');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 20_000 });

    await expect(page.locator('h1', { hasText: 'Tasks' })).toBeVisible();
  });

  test('GET /api/auth/passphrase status returns source after login', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.get('/api/auth/passphrase');
    expect(res.ok()).toBe(true);
    const body = await res.json() as { source: string };
    // In auth mode with server.secret set in YAML, source should be "yaml".
    expect(body.source).toBe('yaml');
  });

  test('POST /api/auth/logout invalidates session', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);

    // Verify we are logged in.
    const tasksRes = await request.get('/api/tasks');
    expect(tasksRes.ok()).toBe(true);

    // Logout.
    const logoutRes = await request.post('/api/auth/logout');
    expect(logoutRes.ok()).toBe(true);

    // Now the session should be invalid.
    const afterRes = await request.get('/api/tasks');
    expect(afterRes.status()).toBe(401);
  });
});
