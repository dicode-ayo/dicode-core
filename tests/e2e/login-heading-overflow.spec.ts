/**
 * login-heading-overflow.spec.ts
 *
 * Regression coverage for #853: `loginTitle` (pkg/webui/server.go) used to
 * build the login page heading by concatenating a webhook task's label with
 * its entire, unbounded `description`. A buildin task's description routinely
 * runs to a paragraph, so on a narrow viewport the resulting <h1> pushed the
 * password field off the fold — and the same string became the window's
 * <title> (and, for a windowed task per #852, its WM_NAME), which is useless
 * as a label at that length.
 *
 * The fixture task `e2e-tests/login-heading-target` (see
 * tests/e2e/fixtures/tasks/login-heading-target/task.yaml) reproduces this
 * with a `trigger.auth: true` webhook and a paragraph-length description.
 *
 * Runs against the 'authenticated' project (server.auth: true, no seeded
 * session — see auth.spec.ts) so /login is reached for real rather than
 * bypassed via storageState.
 */

import { test, expect } from '@playwright/test';

const NEXT = '/hooks/login-heading-target';

test.describe('Login heading overflow (#853)', () => {
  test('GET /api/login/context returns a short title and a bounded subtitle', async ({ request }) => {
    const res = await request.get('/api/login/context?next=' + encodeURIComponent(NEXT));
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as { title?: string; subtitle?: string };

    expect(body.title).toBe('Sign in to Login Heading Target');
    // The full description is 80+ words; the subtitle must be bounded, not
    // the raw field, or this assertion catches the regression directly.
    expect(body.subtitle?.length ?? 0).toBeLessThanOrEqual(121);
    expect(body.subtitle).toMatch(/…$/);
  });

  test('the password field stays on-screen in a narrow window', async ({ browser }) => {
    const context = await browser.newContext({ viewport: { width: 400, height: 700 } });
    const page = await context.newPage();
    await page.goto('/login?next=' + encodeURIComponent(NEXT));

    const subtitle = page.locator('#login-subtitle');
    await expect(subtitle).toBeVisible({ timeout: 10_000 });
    await expect(subtitle).not.toHaveText('');

    // This is the actual bug: before the fix, the heading was 11 lines of
    // prose and the password field was scrolled below the fold.
    await expect(page.locator('input[name=password]')).toBeInViewport();
    await expect(page.getByRole('button', { name: 'Sign in' })).toBeInViewport();

    await context.close();
  });
});
