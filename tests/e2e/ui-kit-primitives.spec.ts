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
    // The h2 carries slot="title" directly — ::slotted() only matches
    // directly-assigned nodes, not their descendants.
    await expect(card.locator('h2[slot="title"]')).toHaveText('Custom title slot');
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

  // `label` is documented as required; an empty one renders aria-label=""
  // — a button with no accessible name, which the empty attribute then
  // hides from tooling that flags nameless buttons. Assert the resolved
  // name rather than the attribute's presence, for every instance on the
  // page, so a future icon-button added without a label fails here.
  test('every dc-icon-button resolves a non-empty accessible name', async ({ page }) => {
    const buttons = page.locator('dc-icon-button');
    const count = await buttons.count();
    expect(count).toBeGreaterThan(0);

    for (let i = 0; i < count; i++) {
      const host = buttons.nth(i);
      const id = await host.getAttribute('id');
      const name = await host.locator('button').evaluate(
        el => el.getAttribute('aria-label')?.trim() ?? '',
      );
      expect(name, `<dc-icon-button id="${id}"> has no accessible name`).not.toBe('');
    }
  });

  test('dc-icon-button reflects the disabled property to its inner <button>', async ({ page }) => {
    const inner = page.locator('dc-icon-button#icon-btn-disabled button');
    await expect(inner).toBeDisabled();
  });

  // Compares computed style, not just attribute presence — asserting only
  // `variant="danger"` is set would stay green even if the CSS behind it
  // were deleted entirely. Focusing each button and comparing outline
  // color would have caught the danger-variant focus-color bug fixed in
  // 1588e999e2106341ef3b201d5dd6210c3628ac5a (danger only tinted :hover,
  // not :focus-visible).
  test('dc-icon-button danger variant renders a distinct focus outline color', async ({ page }) => {
    const defaultBtn = page.locator('dc-icon-button#icon-btn-default button');
    const dangerBtn = page.locator('dc-icon-button#icon-btn-danger button');
    await defaultBtn.focus();
    const defaultOutline = await defaultBtn.evaluate(el => getComputedStyle(el).outlineColor);
    await dangerBtn.focus();
    const dangerOutline = await dangerBtn.evaluate(el => getComputedStyle(el).outlineColor);
    expect(dangerOutline).not.toBe(defaultOutline);
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

  // No fixed-count assertion here deliberately: the demo page derives its
  // pill list from dc-status-badge's own exported KNOWN_STATUSES specifically
  // so a status added there is automatically exercised without a hand-kept
  // count anywhere drifting out of sync — a hardcoded `toHaveCount(9)` would
  // just reintroduce that same drift risk one level up, in the test. The
  // distinct-classes/colors coverage above already exercises the behavior
  // that matters.

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

  // dc-table's shell is <div role="table">/<div role="rowgroup"> rather
  // than literal <table>/<thead>/<tbody> (see dc-table.js's doc comment on
  // why), and its rows/cells are real <tr>/<th>/<td> elements slotted in
  // from the light DOM rather than descendants of an actual <table>. Left to
  // inference that is not portable — the same tree reported `columnheader`
  // for the slotted <th> in one Chromium build and `cell`, i.e. a table with
  // no header semantics, in another — so dc-table declares the roles and
  // this pins the result.
  test('dc-table composes an accessible table tree despite the div/slot shell', async ({ page }) => {
    const table = page.locator('dc-table#table-with-rows');
    await expect(table).toMatchAriaSnapshot(`
      - table:
        - rowgroup:
          - row "ID Name Status":
            - columnheader "ID"
            - columnheader "Name"
            - columnheader "Status"
        - rowgroup:
          - row "task-1 Example One success":
            - cell "task-1"
            - cell "Example One"
            - cell "success"
          - row "task-2 Example Two running":
            - cell "task-2"
            - cell "Example Two"
            - cell "running"
          - row "task-3 Example Three failure":
            - cell "task-3"
            - cell "Example Three"
            - cell "failure"
    `);
  });

  test('dc-table renders its empty state when no rows are slotted', async ({ page }) => {
    const table = page.locator('dc-table#table-empty');
    await expect(table.locator('[role="table"]')).toHaveCount(0);
    const empty = table.locator('dc-empty-state');
    await expect(empty).toBeVisible();
    await expect(empty.locator('.icon')).toHaveText('🗂️');
    await expect(empty.locator('.message')).toHaveText('No rows slotted.');
  });

  // A head row present with zero body rows must still render inside the
  // styled table shell — not as a bare, unstyled <tr> — alongside the
  // empty-state message, not one or the other.
  test('dc-table renders a styled head row alongside the empty state when only the head is slotted', async ({ page }) => {
    const table = page.locator('dc-table#table-head-only');
    await expect(table.locator('[role="table"]')).toBeVisible();
    await expect(table.locator('tr[slot="head"] th').first()).toHaveText('ID');
    await expect(table.locator('tr:not([slot])')).toHaveCount(0);
    const empty = table.locator('dc-empty-state');
    await expect(empty).toBeVisible();
    await expect(empty.locator('.message')).toHaveText('Waiting for data…');
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
