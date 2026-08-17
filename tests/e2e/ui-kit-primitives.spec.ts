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

  // The danger variant only changes hover/focus colors, so this has to
  // drive a real hover — asserting the attribute alone would stay green
  // with the `:host([variant='danger'])` rules deleted outright.
  test('dc-icon-button danger variant hovers to a different color than default', async ({ page }) => {
    const danger = page.locator('dc-icon-button#icon-btn-danger button');
    const plain = page.locator('dc-icon-button#icon-btn-default button');

    await danger.hover();
    const dangerHover = await danger.evaluate(el => {
      const s = getComputedStyle(el);
      return { color: s.color, border: s.borderTopColor };
    });

    await plain.hover();
    const plainHover = await plain.evaluate(el => {
      const s = getComputedStyle(el);
      return { color: s.color, border: s.borderTopColor };
    });

    expect(dangerHover.color).not.toBe(plainHover.color);
    expect(dangerHover.border).not.toBe(plainHover.border);
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

  // Derived from the component's own exported set rather than hand-counted,
  // so adding a status stays a one-line change there instead of silently
  // invalidating a literal here.
  test('the demo renders one pill per known status, plus the unknown-value example', async ({ page }) => {
    const knownCount = await page.evaluate(async (base) => {
      const mod = await import(`${base}/app/components/dc-status-badge.js`);
      return mod.KNOWN_STATUSES.size;
    }, WEBUI_URL);

    expect(knownCount).toBeGreaterThan(0);
    await expect(page.locator('#status-badges dc-status-badge')).toHaveCount(knownCount + 1);
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

  // The shell is <div role="table">, not a real <table>, and the rows are
  // slotted from the light DOM — so whether <tr>/<th>/<td> still map to
  // row/columnheader/cell has to be asserted, not assumed. They do, via
  // the composed tree, with no explicit role attributes on the rows.
  test('dc-table exposes rows and cells to the accessibility tree', async ({ page }) => {
    await expect(page.locator('dc-table#table-with-rows')).toMatchAriaSnapshot(`
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
