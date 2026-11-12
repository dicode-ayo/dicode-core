/**
 * http-register-auth.spec.ts
 *
 * A daemon task's http.register route inherits the task's trigger.auth. The
 * http-register-guarded fixture is a daemon task with `auth: true` that serves
 * /hooks/test-http-register over its own IPC connection; without a session the
 * route must be refused before the request reaches the task.
 *
 * Runs in the 'authenticated' project (server.auth: true).
 */

import { test, expect, APIRequestContext } from '@playwright/test';
import { TEST_PASSPHRASE, login } from './helpers/auth';

const ROUTE = '/hooks/test-http-register';

/** Wait, with a session, until the daemon has claimed its route. */
async function waitForRoute(request: APIRequestContext): Promise<void> {
  await login(request, TEST_PASSPHRASE);
  await expect
    .poll(async () => (await request.post(ROUTE, { data: {} })).status(), { timeout: 20_000 })
    .toBe(200);
}

test.describe('http.register route auth', () => {
  test('with a session the daemon task serves the route', async ({ request }) => {
    await waitForRoute(request);
    const res = await request.post(`${ROUTE}/api/ping`, { data: {} });
    expect(res.status()).toBe(200);
    const body = await res.json();
    expect(body.servedBy).toBe('http-register-guarded');
    expect(body.path).toBe(`${ROUTE}/api/ping`);
  });

  test('without a session POST → 401 and nothing is served', async ({ playwright, request }) => {
    await waitForRoute(request);
    const anon = await playwright.request.newContext({ baseURL: test.info().project.use.baseURL });
    try {
      for (const path of [ROUTE, `${ROUTE}/api/ping`]) {
        const res = await anon.post(path, { data: {} });
        expect(res.status(), path).toBe(401);
        expect(await res.text()).not.toContain('http-register-guarded');
      }
    } finally {
      await anon.dispose();
    }
  });

  test('without a session a browser GET is redirected to /login', async ({ playwright, request }) => {
    await waitForRoute(request);
    const anon = await playwright.request.newContext({ baseURL: test.info().project.use.baseURL });
    try {
      const res = await anon.get(ROUTE, { headers: { Accept: 'text/html' }, maxRedirects: 0 });
      expect(res.status()).toBe(303);
      expect(res.headers()['location']).toBe(`/login?next=${encodeURIComponent(ROUTE)}`);
    } finally {
      await anon.dispose();
    }
  });
});
