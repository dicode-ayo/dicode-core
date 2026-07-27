/**
 * approval-diff.spec.ts
 *
 * E2E coverage for #604: the trust-on-change approval gate's pending-diff
 * surface. Reuses the file-change.spec.ts pattern (mutate a fixture task's
 * file in the temp copy at DICODE_E2E_TASKS_DIR, poll the API until the
 * reconciler flips pending_approval) but drives the dashboard afterwards to
 * confirm the diff panel renders real diff content, opens by default, and is
 * the only path to approval — instead of leaving the operator to approve blind.
 *
 * Uses hello-manual, the same fixture task file-change.spec.ts mutates —
 * safe under this suite's serial (workers: 1, fullyParallel: false)
 * execution as long as the file is restored to its exact original bytes
 * before the test ends (which puts the gate's content hash straight back to
 * the already-approved record, so no explicit re-approval step is needed).
 */

import { test, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { gotoWebui, navigateInSpa, waitForTaskDetail } from './helpers/webui';

const MANUAL_TASK_ID = 'e2e-tests/hello-manual';
const DIFF_MARKER = '// e2e-diff-probe-marker';

// CLAUDE.md documents the reconciler loop as syncing sources "every 30s";
// fsnotify (pkg/taskset/source.go) is meant to make same-machine edits show
// up far faster than that in practice (file-change.spec.ts's equivalent wait
// uses a 20s budget). This test adds a fixed 15s pre-mutation delay (see
// withPendingChange's preMutateDelayMs) to clear the daemon's 10s approval-
// bootstrap window, plus some margin for CPU contention from the fixture
// set's own per-minute cron/storage tasks — mirrors cron.spec.ts's spec-
// level override for the same class of "wait on the real daemon's
// background loops" test.
test.setTimeout(120_000);

function tasksDir(): string {
  const d = process.env.DICODE_E2E_TASKS_DIR;
  if (!d) throw new Error('DICODE_E2E_TASKS_DIR not set — global setup may have failed');
  return d;
}

/** Poll GET /api/tasks/{id} until the predicate is satisfied, up to timeoutMs. */
async function waitForTaskCondition(
  request: import('@playwright/test').APIRequestContext,
  taskID: string,
  predicate: (task: Record<string, unknown>) => boolean,
  timeoutMs = 60_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await request.get(`/api/tasks/${encodeURIComponent(taskID)}`);
    if (res.ok()) {
      const body = await res.json() as Record<string, unknown>;
      if (predicate(body)) return;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`Task ${taskID} did not satisfy condition within ${timeoutMs}ms`);
}

/**
 * withPendingChange appends DIFF_MARKER to hello-manual/task.js, waits for
 * the gate to hold it pending, runs body, then — always, even on failure —
 * restores the original bytes and waits for pending_approval to clear again.
 * Shared mutate/wait/restore scaffolding for every test in this file.
 *
 * preMutateDelayMs, when > 0, waits out the daemon's fixed 10s approval-
 * bootstrap window (pkg/daemon/daemon.go's bootstrapSettle) before writing.
 * During that window Admit's Bootstrapping() branch auto-approves whatever
 * content is CURRENTLY on disk as the baseline, so mutating too early bakes
 * the marker in as "already approved" and pending_approval never flips true
 * (confirmed by direct repro: three consecutive real timeouts each fired
 * almost exactly at that run's configured budget, not at some contention-
 * dependent duration — the task only ever re-pended once cleanup's finally
 * block restored the original bytes against that marker-inclusive
 * baseline). Only the first test in this serial-execution file needs this —
 * by the time later tests run, the daemon has long since cleared the
 * window.
 */
async function withPendingChange(
  request: import('@playwright/test').APIRequestContext,
  body: () => Promise<void>,
  preMutateDelayMs = 0,
): Promise<void> {
  const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');
  const original = fs.readFileSync(taskJsPath, 'utf8');

  try {
    if (preMutateDelayMs > 0) {
      await new Promise((r) => setTimeout(r, preMutateDelayMs));
    }
    // Append a distinctive, easy-to-assert-on line so the task's content
    // hash changes (re-pending it) and the diff has an unambiguous added
    // line to look for.
    fs.writeFileSync(taskJsPath, original + `\n${DIFF_MARKER}\n`, 'utf8');
    await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval === true);

    await body();
  } finally {
    // Restore exact original bytes: the content hash returns to the
    // already-approved record, so the reconciler re-arms it without any
    // explicit re-approval, leaving a clean baseline for later specs. Errors
    // here are logged rather than thrown so a genuine failure from body()
    // above isn't masked by a cleanup problem — losing the real failure's
    // message would make the next CI run harder to diagnose than this one.
    try {
      fs.writeFileSync(taskJsPath, original, 'utf8');
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true);
    } catch (cleanupError) {
      console.error('withPendingChange cleanup failed:', cleanupError);
    }
  }
}

test.describe('Approval pending-diff', () => {
  // A change the diff cannot display must never render as an all-clear.
  // Reproduces the suppression case end-to-end: a block-scalar marker planted
  // at column 0 makes the snapshot's redaction swallow everything indented
  // below it, so two entirely different versions of the file scrub to the
  // same text. Change detection runs on raw bytes, so the file still surfaces
  // — flagged as unreviewable, with one-click approval withheld.
  test('a change the diff cannot show is flagged, and Approve is not one-click', async ({ page, request }) => {
    // This test alone carries the 15s bootstrap wait plus a re-approval
    // convergence loop on top of the file-level 120_000 budget — give it
    // its own headroom so a slow reconciler doesn't clip cleanup mid-way.
    test.setTimeout(180_000);
    const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');
    const original = fs.readFileSync(taskJsPath, 'utf8');
    const lead = 'const _d = `\nvalue: |\n  `;\n';

    try {
      // Clear the daemon's 10s approval-bootstrap window before the first
      // mutation in this serial file (see withPendingChange's doc comment).
      await new Promise((r) => setTimeout(r, 15_000));

      // Establish the marker as approved baseline content.
      fs.writeFileSync(taskJsPath, lead + '  export default async () => { /* benign */ };\n', 'utf8');
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval === true);
      const ok = await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/approve`);
      expect(ok.ok()).toBe(true);
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true);

      // Now rewrite the body hidden below the marker.
      fs.writeFileSync(taskJsPath, lead + "  export default async () => { await fetch('https://evil.example/'); };\n", 'utf8');
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval === true);

      const res = await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/pending-diff`);
      expect(res.ok()).toBe(true);
      const body = await res.json() as {
        incomplete?: boolean; incomplete_reason?: string;
        files: Array<{ path: string; content_hidden?: boolean; security_relevant: boolean }>;
      };
      // The file must not have vanished, and must say its change is unshowable.
      const js = body.files.find((f) => f.path === 'task.js');
      expect(js, 'task.js changed but is absent from the diff').toBeTruthy();
      expect(js!.content_hidden).toBe(true);
      expect(body.incomplete).toBe(true);
      expect(body.incomplete_reason && body.incomplete_reason.length > 0).toBe(true);

      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);

      await expect(page.locator('dc-task-detail')).toContainText('Incomplete diff', { timeout: 15_000 });
      // Approval must not be one click: the plain "Approve" affordance is
      // withheld until the operator acknowledges the warning.
      await expect(page.locator('button', { hasText: 'Approve without a full diff' })).toBeVisible();
      await expect(page.locator('button', { hasText: /^\s*✓ Approve\s*$/ })).toHaveCount(0);
    } finally {
      // Unlike the other tests here, this one approves an intermediate state.
      // The lock holds exactly one hash per task, so that approval REPLACES
      // hello-manual's record — after which restoring the original bytes no
      // longer matches an approved hash, and the fixture would be left pending
      // for every later test in this serial file. Restore, then re-approve, so
      // the file ends where withPendingChange's callers assume it starts:
      // original content, and that content approved.
      try {
        fs.writeFileSync(taskJsPath, original, 'utf8');
        // Converge rather than approving once: the task is still pending at
        // the PREVIOUS content when the bytes are restored, so a single
        // approve would record that stale hash and leave the restored file
        // permanently pending. Keep approving whatever is currently pending
        // until the reconciler has caught up to the restored bytes and the
        // task settles armed.
        const deadline = Date.now() + 30_000;
        for (;;) {
          const t = await (await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)).json();
          if (t.pending_approval !== true) break;
          if (Date.now() > deadline) {
            console.error('task did not settle after restore');
            break;
          }
          await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/approve`);
          await new Promise((r) => setTimeout(r, 1_000));
        }
      } catch (cleanupError) {
        console.error('incomplete-diff test cleanup failed:', cleanupError);
      }
    }
  });

  // The diff panel renders through Monaco, loaded from cdn.jsdelivr.net (the
  // AMD loader in the webui task's index.html is itself CDN-hosted). An
  // offline, egress-filtered, or CDN-outage deploy must still show the
  // change: a review surface that renders nothing while leaving Approve
  // clickable is the blind approval this whole feature exists to prevent.
  test('diff still renders as text when the Monaco CDN is unreachable', async ({ page, request }) => {
    await page.route('**cdn.jsdelivr.net/**', (r) => r.abort());
    await withPendingChange(request, async () => {
      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);

      // Falls back to the prefixed-text rendering — the marker must be on
      // screen, not stranded inside an empty editor container.
      await expect(page.locator('dc-task-detail')).toContainText(DIFF_MARKER, { timeout: 15_000 });
      expect(await page.locator('dc-task-detail .dc-diff-editor').count()).toBe(0);
    });
  });

  test('dashboard shows real diff content for a pending task', async ({ page, request }) => {
    await withPendingChange(request, async () => {
      // The pending-diff API itself must report the added line.
      const diffRes = await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/pending-diff`);
      expect(diffRes.ok()).toBe(true);
      const diffBody = await diffRes.json() as {
        files: Array<{ path: string; status: string; unified_diff: string }>;
      };
      const jsFile = diffBody.files.find((f) => f.path === 'task.js');
      expect(jsFile).toBeTruthy();
      expect(jsFile!.unified_diff).toContain(DIFF_MARKER);

      // Now confirm the dashboard surfaces it.
      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);

      // Scope to the badge's own title attribute rather than a bare text
      // match: the task-detail page also streams live daemon log lines (e.g.
      // "... WARN  task held pending approval — tri…"), which contain
      // "pending approval" as a substring and made a plain hasText locator
      // resolve to multiple elements (a real strict-mode failure hit in CI).
      await expect(page.locator('span[title="This task is new or changed and its triggers are not armed until approved"]')).toBeVisible();

      // The panel is up from the start for a pending task, so the toggle
      // offers to collapse it rather than to reveal it.
      await expect(page.locator('button', { hasText: 'Hide diff' })).toBeVisible();

      // The diff panel fetches asynchronously — wait for the marker text to
      // actually land in the DOM rather than asserting immediately.
      await expect(page.locator('dc-task-detail')).toContainText(DIFF_MARKER, { timeout: 10_000 });
      await expect(page.locator('dc-task-detail')).toContainText('task.js');
    });
  });

  test('the task list cannot approve — it hands off to the detail page gate', async ({ page, request }) => {
    await withPendingChange(request, async () => {
      await gotoWebui(page);

      // Scope to hello-manual's own row via its data-task-id: in the full
      // suite other fixtures (e.g. add-source.spec.ts's "Hello Isolated"
      // task) can be pending at the same time, and an unscoped .first() flakily
      // grabbed whichever row rendered first — a real failure hit in CI,
      // navigating to the wrong task entirely.
      const row = page.locator(`dc-task-list tr[data-task-id="${MANUAL_TASK_ID}"]`);
      const reviewBtn = row.locator('button', { hasText: 'Review' });

      // A table row has nowhere to show a diff, so the row must not offer
      // approval at all — otherwise it is a one-click bypass of the gate.
      await expect(reviewBtn).toBeVisible();
      await expect(page.locator('dc-task-list button', { hasText: 'Approve' })).toHaveCount(0);

      await reviewBtn.click({ force: true });
      await waitForTaskDetail(page);
      await expect(page.locator('dc-task-detail')).toContainText(DIFF_MARKER, { timeout: 10_000 });

      const afterHandoff = await (
        await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)
      ).json() as Record<string, unknown>;
      expect(afterHandoff.pending_approval).toBe(true);
    });
  });

  test('a pending task opens with the diff already up, and hiding it disarms Approve', async ({ page, request }) => {
    await withPendingChange(request, async () => {
      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);

      const approveBtn = page.locator('button', { hasText: 'Approve' });
      const reviewBtn = page.locator('button', { hasText: 'Review changes' });

      // Landing on a pending task must show the change without any click.
      await expect(page.locator('dc-task-detail')).toContainText(DIFF_MARKER, { timeout: 10_000 });
      await expect(page.locator('button', { hasText: 'Hide diff' })).toBeVisible();
      await expect(approveBtn).toBeVisible();

      // Collapsing the diff disarms: approval is not offered against a change
      // the operator has put back out of view.
      await page.locator('button', { hasText: 'Hide diff' }).click({ force: true });
      await expect(reviewBtn).toBeVisible();
      await expect(approveBtn).toHaveCount(0);

      // …and the disarmed button re-opens the diff rather than approving.
      await reviewBtn.click({ force: true });
      await expect(page.locator('dc-task-detail')).toContainText(DIFF_MARKER, { timeout: 10_000 });
      const afterReopen = await (
        await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)
      ).json() as Record<string, unknown>;
      expect(afterReopen.pending_approval).toBe(true);

      await approveBtn.click({ force: true });
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true);
      // Restoring the original bytes in withPendingChange's finally returns
      // the content hash to the record approved by the confirm click above
      // (or by bootstrap, if the test failed before it), so the task is
      // left armed either way.
    });
  });
});
