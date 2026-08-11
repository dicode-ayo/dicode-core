/**
 * ui-kit-primitives.spec.ts
 *
 * E2E regression coverage for the Stage 1 (#93) Lit primitives:
 * dc-card, dc-page-header, dc-empty-state, dc-icon-button, dc-status-badge,
 * dc-table — plus the demo page (dc-ui-kit-demo) that mounts them at
 * /hooks/webui/ui-kit.
 *
 * Runs under the `webui` Playwright project (registered in
 * playwright.config.ts alongside webui-task.spec.ts) since the route lives
 * inside the auth-gated SPA and needs the same seeded session.
 *
 * Playwright locators pierce open shadow roots by default, so these specs
 * query straight through each primitive's Shadow DOM without any special
 * shadow-piercing syntax.
 */

import { test, expect } from '@playwright/test';

const WEBUI_URL = '/hooks/webui';
const UI_KIT_URL = `${WEBUI_URL}/ui-kit`;

/** Navigate to the ui-kit demo page and wait for it to mount. */
async function gotoUiKit(page: import('@playwright/test').Page) {
  await page.goto(UI_KIT_URL);
  await page.waitForSelector('dc-ui-kit-demo', { timeout: 10_000 });
  // Sanity check the SPA router actually resolved /ui-kit (rather than
  // falling through to the 404 handler) before any assertions run.
  await expect(page.locator('dc-page-header')).toBeVisible();
}

test.describe('UI Kit primitives (#93 Stage 1)', () => {
  test.beforeEach(async ({ page }) => {
    await gotoUiKit(page);
  });

  test('demo page loads at /hooks/webui/ui-kit', async ({ page }) => {
    expect(page.url()).toContain(UI_KIT_URL);
    await expect(page.locator('dc-ui-kit-demo')).toBeVisible();
  });

  // ── dc-page-header ──────────────────────────────────────────────────────

  test('dc-page-header renders heading, subtitle, and slotted actions', async ({ page }) => {
    const header = page.locator('dc-page-header');
    await expect(header.locator('h1')).toHaveText('UI Kit');
    await expect(header.locator('.subtitle')).toContainText('Stage 1 primitives');
    await expect(header.locator('button', { hasText: 'Reload' })).toBeVisible();
  });

  // ── dc-card ──────────────────────────────────────────────────────────────

  test('dc-card renders default body content', async ({ page }) => {
    const card = page.locator('dc-card[heading="dc-status-badge"]');
    await expect(card).toBeVisible();
    await expect(card.locator('.header h2')).toHaveText('dc-status-badge');
  });

  test('dc-card projects title and actions slot content', async ({ page }) => {
    const card = page.locator('dc-card#card-slots-demo');
    await expect(card).toBeVisible();
    // Slot content lives in the light DOM (projected), title/actions are
    // custom slotted elements rather than the `heading` prop's fallback <h2>.
    await expect(card.locator('[slot="title"] h2')).toHaveText('Custom title slot');
    await expect(card.locator('[slot="actions"] button')).toHaveText('Action');
    await expect(card.locator('p')).toContainText('title');
  });

  // ── dc-empty-state ───────────────────────────────────────────────────────

  test('dc-empty-state renders icon, message, and CTA slot', async ({ page }) => {
    const empty = page.locator('dc-empty-state#empty-state-demo');
    await expect(empty).toBeVisible();
    await expect(empty.locator('.icon')).toHaveText('📭');
    await expect(empty.locator('.message')).toHaveText('No notifications yet.');
    await expect(empty.locator('button', { hasText: 'Create one' })).toBeVisible();
  });

  // ── dc-icon-button ───────────────────────────────────────────────────────

  test('dc-icon-button exposes an accessible name via aria-label on a real <button>', async ({ page }) => {
    const btn = page.locator('dc-icon-button#icon-btn-default');
    const inner = btn.locator('button');
    await expect(inner).toHaveAttribute('aria-label', 'Refresh');
    await expect(inner).not.toBeDisabled();
  });

  test('dc-icon-button reflects the disabled property to its inner <button>', async ({ page }) => {
    const inner = page.locator('dc-icon-button#icon-btn-disabled button');
    await expect(inner).toBeDisabled();
  });

  test('dc-icon-button danger variant is a distinct visual variant from default', async ({ page }) => {
    const danger = page.locator('dc-icon-button#icon-btn-danger');
    await expect(danger).toHaveAttribute('variant', 'danger');
    const defaultBtn = page.locator('dc-icon-button#icon-btn-default');
    await expect(defaultBtn).not.toHaveAttribute('variant', 'danger');
  });

  // ── dc-status-badge ──────────────────────────────────────────────────────

  test('dc-status-badge renders distinct classes for different status values', async ({ page }) => {
    const group = page.locator('#status-badges');
    const success = group.locator('dc-status-badge[status="success"] .badge');
    const failure = group.locator('dc-status-badge[status="failure"] .badge');
    const unknown = group.locator('dc-status-badge[status="unknown-status"] .badge');

    await expect(success).toHaveText('success');
    await expect(failure).toHaveText('failure');
    await expect(unknown).toHaveText('unknown-status');

    await expect(success).toHaveClass(/badge-success/);
    await expect(failure).toHaveClass(/badge-failure/);
    // An unrecognized status value falls back to the neutral style — no
    // badge-<status> modifier class, and visually distinct from a known one.
    await expect(unknown).not.toHaveClass(/badge-success/);
    await expect(unknown).not.toHaveClass(/badge-failure/);

    const successColor = await success.evaluate(el => getComputedStyle(el).color);
    const failureColor = await failure.evaluate(el => getComputedStyle(el).color);
    expect(successColor).not.toBe(failureColor);
  });

  test('dc-status-badge status values total nine example pills', async ({ page }) => {
    await expect(page.locator('#status-badges dc-status-badge')).toHaveCount(9);
  });

  // ── dc-table ─────────────────────────────────────────────────────────────

  test('dc-table renders slotted head and body rows', async ({ page }) => {
    const table = page.locator('dc-table#table-with-rows');
    // The projected <tr>/<th>/<td> elements are real light-DOM children of
    // the <dc-table> host (slotting only changes where they *paint*, not
    // where they live in the DOM tree) so these are queried directly,
    // without needing to reach through the shadow root's <slot>.
    await expect(table.locator('[role="table"]')).toBeVisible(); // shadow-root shell rendered
    await expect(table.locator('tr[slot="head"] th').first()).toHaveText('ID');
    const bodyRows = table.locator('tr:not([slot])');
    await expect(bodyRows).toHaveCount(3);
    await expect(bodyRows.first()).toContainText('task-1');
    // A body cell projects a nested primitive (dc-status-badge) correctly —
    // reaching into *that* component's own shadow root (a single, ordinary
    // shadow-pierce, not a slot-flattening hop) works as usual.
    await expect(bodyRows.first().locator('dc-status-badge .badge')).toHaveText('success');
  });

  test('dc-table renders its empty state when no rows are slotted', async ({ page }) => {
    const table = page.locator('dc-table#table-empty');
    await expect(table.locator('[role="table"]')).toHaveCount(0);
    const empty = table.locator('dc-empty-state');
    await expect(empty).toBeVisible();
    await expect(empty.locator('.icon')).toHaveText('🗂️');
    await expect(empty.locator('.message')).toHaveText('No rows slotted.');
  });

  // ── DcElement base class (Stage 2 plumbing, dogfooded on this page) ─────

  test('DcElement _fetch() drives visible loading and success state', async ({ page }) => {
    await page.locator('#simulate-ok').click();
    await expect(page.locator('#demo-loading')).toBeVisible();
    await expect(page.locator('#demo-result')).toHaveText('simulated data loaded OK', { timeout: 5_000 });
    await expect(page.locator('#demo-loading')).toHaveCount(0);
  });

  test('DcElement _fetch() captures a rejected call into _error', async ({ page }) => {
    await page.locator('#simulate-fail').click();
    await expect(page.locator('#demo-error')).toContainText('simulated failure', { timeout: 5_000 });
  });
});
