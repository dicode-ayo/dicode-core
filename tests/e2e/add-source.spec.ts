/**
 * add-source.spec.ts
 *
 * Regression coverage for issue #553: the config page's "Add source" form
 * never sent a `name` field, so every submission — local or git — failed
 * backend validation with 400 "name is required". These tests drive the
 * real DOM form (fill inputs, click the button) rather than hand-building a
 * fetch body, so a regression in the form's wiring (e.g. the name input
 * losing its id, or `_addSource()` dropping the field again) is caught the
 * same way a user would hit it.
 *
 * Runs in the unauthenticated project (server.auth=false) alongside
 * config.spec.ts, which documents the same auth posture.
 *
 * Issue #621: the first test below used to point its local-source add at
 * DICODE_E2E_TASKSET_PATH — the EXACT SAME taskset.yaml the fixture-seeded
 * "e2e-tests" source (tests/e2e/fixtures/dicode-unauth.yaml) already watches.
 * Adding a second source pointed at a path an existing source is already
 * watching let the two sources collide on the reconciler's internal
 * Source.ID()-keyed teardown bookkeeping (fixed separately in
 * pkg/webui/server.go / pkg/daemon/daemon.go via taskset.SourceID), and
 * caused cron.spec.ts's "task list shows last run status after cron fires"
 * test — which runs in the same file-alphabetical order right after this one
 * in the shared "unauthenticated" project daemon — to fail with a 90s
 * timeout as e2e-tests/hello-cron cycled register/unregister. It now uses
 * DICODE_E2E_ADD_SOURCE_TASKSET_PATH, a dedicated fixture
 * (tests/e2e/fixtures/add-source-tasks/) in its own separate temp directory
 * that no other source ever watches.
 */

import { test, expect } from '@playwright/test';
import { gotoWebui, navigateInSpa, waitForConfigPage } from './helpers/webui';
import type { Page } from '@playwright/test';

async function openConfigPage(page: Page): Promise<void> {
  await gotoWebui(page);
  await navigateInSpa(page, '/config');
  await waitForConfigPage(page);
  // Reveal the add-source form — it starts collapsed inside <details>.
  await page.locator('dc-config summary', { hasText: 'Add source' }).click();
  await expect(page.locator('#new-src-name')).toBeVisible();
}

test.describe('Config UI — Add source', () => {
  test('adds a local source through the real form and it appears via the API', async ({ page, request }) => {
    // A dedicated, isolated fixture (issue #621) — never the same path/watch
    // root as the "e2e-tests" source already seeded by dicode-unauth.yaml.
    const tasksetPath = process.env.DICODE_E2E_ADD_SOURCE_TASKSET_PATH;
    test.skip(!tasksetPath, 'DICODE_E2E_ADD_SOURCE_TASKSET_PATH not set — cannot exercise a local source add');

    const name = `e2e-add-local-${Date.now()}`;

    await openConfigPage(page);

    // Type defaults to "local" — its field is the one already visible.
    await page.locator('#new-src-name').fill(name);
    await page.locator('#new-src-path').fill(tasksetPath!);
    await page.locator('dc-config button', { hasText: 'Add source' }).click();

    // The UI's own "Sources" table reads a config field the frontend never
    // actually populates (this._cfg.Sources doesn't exist on the API
    // response — a separate, pre-existing display bug outside this issue's
    // scope), so GET /api/sources — the same endpoint SourceManager.List
    // backs and the one the MCP tooling and other e2e specs already treat as
    // the source of truth — is the reliable oracle for "was it added".
    await expect(async () => {
      const res = await request.get('/api/sources');
      expect(res.ok()).toBe(true);
      const sources = await res.json() as Array<Record<string, unknown>>;
      expect(sources.some((s) => (s.name ?? s.Name) === name)).toBe(true);
    }).toPass({ timeout: 10_000 });

    // No leftover error status from a failed submit.
    const status = await page.locator('dc-config').textContent();
    expect(status).not.toContain('name is required');
    expect(status).not.toContain('Error:');
  });

  test('rejects a duplicate source name from the form with a clear error', async ({ page }) => {
    const tasksetPath = process.env.DICODE_E2E_TASKSET_PATH;
    test.skip(!tasksetPath, 'DICODE_E2E_TASKSET_PATH not set — cannot exercise a local source add');

    // "e2e-tests" is the fixture-seeded source name (tests/e2e/fixtures/dicode-unauth.yaml).
    await openConfigPage(page);
    await page.locator('#new-src-name').fill('e2e-tests');
    await page.locator('#new-src-path').fill(tasksetPath!);
    await page.locator('dc-config button', { hasText: 'Add source' }).click();

    await expect(page.locator('dc-config')).toContainText(/already exists/i, { timeout: 10_000 });
  });

  test('sends the name field for a git source too', async ({ page }) => {
    // Drives the git branch of the form (type toggle, url/branch/token
    // inputs) to prove `name` is sent regardless of source type. Uses the
    // git:// scheme, which apiAddSource rejects synchronously before any
    // network access (see server.go isAllowedGitScheme / #486) — so this
    // stays fast and hermetic while still confirming the request reaches
    // the handler with a name and gets a scheme error, not "name is
    // required".
    const name = `e2e-add-git-${Date.now()}`;

    await openConfigPage(page);
    await page.locator('#new-src-type').selectOption('git');
    await page.locator('#new-src-name').fill(name);
    await page.locator('#new-src-url').fill('git://github.com/example/repo.git');
    await page.locator('dc-config button', { hasText: 'Add source' }).click();

    const status = await page.locator('dc-config').textContent();
    expect(status).not.toContain('name is required');
    await expect(page.locator('dc-config')).toContainText(/scheme/i, { timeout: 10_000 });
  });
});
