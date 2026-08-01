/**
 * login-no-passphrase.spec.ts
 *
 * Runs against the 'unauthenticated' project (server.auth: false, no
 * passphrase configured — see tests/e2e/helpers/dicode-server.ts). In this
 * mode apiSecretsUnlock (POST /api/auth/login) accepts any password,
 * including empty, because passphraseSource() reports "none". Before
 * dicode-core#639, the static /login page still rendered a password field
 * with an unconditional `required` attribute and no indication that nothing
 * is actually being checked — misleading, and it blocked a truly-empty
 * client-side submission the server would have accepted.
 *
 * This suite locks in that GET /api/login/context now reports
 * `passphrase_required: false` in this mode, that the login page reflects
 * it (no `required` attribute, an honest notice shown), and that submitting
 * the form with an empty password field actually succeeds — the field is
 * genuinely optional, not just cosmetically un-required.
 */

import { test, expect } from '@playwright/test';

test.describe('Login page — no passphrase configured', () => {
  test('GET /api/login/context reports passphrase_required: false', async ({ request }) => {
    const res = await request.get('/api/login/context');
    expect(res.ok()).toBe(true);
    const body = await res.json() as { passphrase_required?: boolean };
    expect(body.passphrase_required).toBe(false);
  });

  test('login page drops the required attribute and shows an honest notice', async ({ page }) => {
    await page.goto('/login');

    const password = page.locator('input[name=password]');
    const note = page.locator('#dc-note');

    // The static HTML ships `required` as the safe default before JS runs;
    // login.js must remove it once /api/login/context confirms no
    // passphrase actually gates this instance.
    await expect(note).toBeVisible({ timeout: 10_000 });
    await expect(note).toContainText('No password is configured');
    await expect(password).not.toHaveAttribute('required', '');
  });

  test('submitting the login form with an empty password succeeds', async ({ page }) => {
    await page.goto('/login');
    await expect(page.locator('#dc-note')).toBeVisible({ timeout: 10_000 });

    // Leave the password field untouched (empty) and submit directly — this
    // is exactly the browser-blocked case #639 describes: with `required`
    // still present this click would be swallowed by native HTML5
    // validation and the page would never navigate.
    await page.getByRole('button', { name: 'Sign in' }).click();

    await page.waitForURL((url) => !url.pathname.startsWith('/login'), { timeout: 10_000 });
  });
});
