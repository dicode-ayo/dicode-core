/**
 * approval-review.spec.ts
 *
 * E2E coverage for the trust-on-change approval gate's review surface: the
 * resolved end state of a pending task — what will run if the operator arms
 * it. Reuses the file-change.spec.ts pattern (mutate a fixture task's file in
 * the temp copy at DICODE_E2E_TASKS_DIR, poll the API until the reconciler
 * flips pending_approval) but drives the dashboard afterwards to confirm the
 * panel renders the resolved task, opens by default, and is the only path to
 * approval — instead of leaving the operator to approve blind.
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
const BODY_MARKER = '// e2e-review-probe-marker';

// CLAUDE.md documents the reconciler loop as syncing sources "every 30s";
// fsnotify (pkg/taskset/source.go) is meant to make same-machine edits show
// up far faster than that in practice (file-change.spec.ts's equivalent wait
// uses a 20s budget). The first test adds a fixed 15s pre-mutation delay (see
// withPendingChange's preMutateDelayMs) to clear the daemon's 10s approval-
// bootstrap window, plus some margin for CPU contention from the fixture
// set's own per-minute cron/storage tasks — mirrors cron.spec.ts's spec-
// level override for the same class of "wait on the real daemon's
// background loops" test.
test.setTimeout(180_000);

type PendingState = {
  task_id: string;
  pending_hash: string;
  runtime?: string;
  triggers?: Array<{ kind: string; cron?: string; webhook?: string }>;
  permissions?: { net?: string[]; run?: string[] };
  env?: Array<{ name: string; kind: string; ref?: string }>;
  files?: Array<{ path: string; kind: string; size?: number; hash?: string }>;
};

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
 * withPendingChange appends BODY_MARKER to hello-manual/task.js, waits for
 * the gate to hold it pending, runs body, then — always, even on failure —
 * restores the original bytes and waits for pending_approval to clear again.
 * Shared mutate/wait/restore scaffolding for every test in this file.
 *
 * preMutateDelayMs, when > 0, waits out the daemon's fixed 10s approval-
 * bootstrap window (pkg/daemon/daemon.go's bootstrapSettle) before writing.
 * During that window Admit's Bootstrapping() branch auto-approves whatever
 * content is CURRENTLY on disk as the baseline, so mutating too early bakes
 * the marker in as "already approved" and pending_approval never flips true.
 * Only the first test in this serial-execution file needs this — by the time
 * later tests run, the daemon has long since cleared the window.
 */
async function withPendingChange(
  request: import('@playwright/test').APIRequestContext,
  body: (ctx: { mutatedSize: number }) => Promise<void>,
  preMutateDelayMs = 0,
): Promise<void> {
  const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');
  const original = fs.readFileSync(taskJsPath, 'utf8');
  const mutated = original + `\n${BODY_MARKER}\n`;

  let bodyError: unknown;
  try {
    if (preMutateDelayMs > 0) {
      await new Promise((r) => setTimeout(r, preMutateDelayMs));
    }
    // Append a distinctive line so the task's content hash changes (re-pending
    // it) and the inventory has an unambiguous size/hash change to assert on.
    fs.writeFileSync(taskJsPath, mutated, 'utf8');
    await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval === true);

    await body({ mutatedSize: Buffer.byteLength(mutated, 'utf8') });
  } catch (err) {
    bodyError = err;
  }

  // Restore exact original bytes: the content hash returns to the
  // already-approved record, so the reconciler re-arms it without any explicit
  // re-approval. Only a test that approves the mutated content needs more than
  // this (see settleToRestored).
  //
  // A cleanup that fails leaves hello-manual pending for every later test and
  // spec file, so it fails the test rather than being logged away — silence
  // here turns one broken fixture into a cascade of unrelated-looking failures
  // elsewhere. A genuine body failure still wins, being the more informative of
  // the two.
  let cleanupError: unknown;
  try {
    fs.writeFileSync(taskJsPath, original, 'utf8');
    await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true);
  } catch (err) {
    cleanupError = err;
  }
  if (bodyError) throw bodyError;
  if (cleanupError) throw cleanupError;
}

/**
 * settleToRestored returns hello-manual to "original content, approved" after a
 * test that clicked Approve.
 *
 * That click records the mutated content's hash, replacing the record the
 * restored bytes match, so the restore re-pends the task — but not
 * instantly: the reconciler has to observe the write first. Polling for
 * "not pending" and exiting on the first hit therefore reports success while
 * the re-pend is still in flight, stranding the task for later spec files.
 *
 * So this requires the task to be observed settled on several consecutive
 * polls spanning more than one reconcile, approving whatever is pending in
 * between. It tolerates the test having failed before its approve click, in
 * which case no re-pend comes and the consecutive-clean check simply passes.
 */
async function settleToRestored(
  request: import('@playwright/test').APIRequestContext,
): Promise<void> {
  const deadline = Date.now() + 90_000;
  const cleanRunNeeded = 8; // 8 polls x 2s = 16s of quiet, well past an fsnotify re-pend
  let clean = 0;
  while (Date.now() < deadline) {
    const t = await (await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)).json() as Record<string, unknown>;
    if (t.pending_approval === true) {
      clean = 0;
      await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/approve`);
    } else if (++clean >= cleanRunNeeded) {
      return;
    }
    await new Promise((r) => setTimeout(r, 2_000));
  }
  throw new Error('hello-manual did not settle to the restored content within 90s');
}

test.describe('Approval review surface', () => {
  test('pending-state reports the resolved task and its file inventory, without code', async ({ request }) => {
    // This test alone carries the 15s bootstrap wait on top of the file-level
    // budget — give it headroom so a slow reconciler doesn't clip cleanup.
    test.setTimeout(240_000);
    await withPendingChange(request, async ({ mutatedSize }) => {
      const res = await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/pending-state`);
      expect(res.ok()).toBe(true);
      const state = await res.json() as PendingState;

      expect(state.task_id).toBe(MANUAL_TASK_ID);
      expect(state.pending_hash).toBeTruthy();
      expect(state.runtime).toBeTruthy();
      expect(state.triggers?.some((t) => t.kind === 'manual')).toBe(true);

      // The inventory carries the changed file as a name, a size and a hash.
      const js = state.files?.find((f) => f.path === 'task.js');
      expect(js, 'task.js missing from the inventory').toBeTruthy();
      expect(js!.kind).toBe('file');
      expect(js!.size).toBe(mutatedSize);
      expect(js!.hash).toMatch(/^[0-9a-f]{64}$/);

      // No code bytes anywhere in the payload.
      expect(JSON.stringify(state)).not.toContain(BODY_MARKER);
    }, 15_000);
  });

  test('the panel renders with no CDN reachable', async ({ page, request }) => {
    // The surface renders structured fields, so displaying them must not
    // depend on any CDN-hosted editor.
    await page.route('**cdn.jsdelivr.net/**', (r) => r.abort());
    await withPendingChange(request, async () => {
      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);

      await expect(page.locator('dc-task-detail')).toContainText('Runs as', { timeout: 15_000 });
      await expect(page.locator('dc-task-detail')).toContainText('task.js');
    });
  });

  test('the task list cannot approve — it hands off to the detail page gate', async ({ page, request }) => {
    await withPendingChange(request, async () => {
      await gotoWebui(page);

      // Scope to hello-manual's own row via its data-task-id: in the full
      // suite other fixtures (e.g. add-source.spec.ts's "Hello Isolated"
      // task) can be pending at the same time, so an unscoped .first()
      // resolves to whichever row rendered first.
      const row = page.locator(`dc-task-list tr[data-task-id="${MANUAL_TASK_ID}"]`);
      const reviewBtn = row.locator('button', { hasText: 'Review' });

      // A table row has nowhere to show a review, so the row must not offer
      // approval at all — otherwise it is a one-click bypass of the gate.
      await expect(reviewBtn).toBeVisible();
      await expect(page.locator('dc-task-list button', { hasText: 'Approve' })).toHaveCount(0);

      await reviewBtn.click({ force: true });
      await waitForTaskDetail(page);
      await expect(page.locator('dc-task-detail')).toContainText('Runs as', { timeout: 10_000 });

      const afterHandoff = await (
        await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)
      ).json() as Record<string, unknown>;
      expect(afterHandoff.pending_approval).toBe(true);
    });
  });

  test('the review panel is up on arrival, and hiding it disarms Approve', async ({ page, request }) => {
    const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');
    const original = fs.readFileSync(taskJsPath, 'utf8');
    let bodyError: unknown;
    try {
      fs.writeFileSync(taskJsPath, original + `\n${BODY_MARKER}\n`, 'utf8');
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval === true);

      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);

      // Scope to the badge's own title attribute rather than a bare text
      // match: the page also streams live daemon log lines containing
      // "pending approval" as a substring, which a plain hasText locator
      // resolves to under strict mode.
      await expect(page.locator('span[title="This task is new or changed and its triggers are not armed until approved"]')).toBeVisible();

      // Landing on a pending task must show what will run without any click.
      await expect(page.locator('dc-task-detail')).toContainText('task.js', { timeout: 15_000 });
      await expect(page.locator('dc-task-detail')).toContainText('Runs as');
      const hideBtn = page.locator('button', { hasText: 'Hide review' });
      await expect(hideBtn).toBeVisible();

      const approveBtn = page.locator('button', { hasText: /^\s*✓ Approve\s*$/ });
      await expect(approveBtn).toBeVisible();

      // Collapsing disarms: approval is not offered against a review the
      // operator has put back out of view.
      await hideBtn.click({ force: true });
      const reviewBtn = page.locator('button', { hasText: /^\s*✎ Review\s*$/ });
      await expect(reviewBtn).toBeVisible();
      await expect(approveBtn).toHaveCount(0);

      // …and the disarmed button re-opens the panel rather than approving.
      await reviewBtn.click({ force: true });
      await expect(page.locator('dc-task-detail')).toContainText('task.js', { timeout: 10_000 });
      const afterReopen = await (
        await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)
      ).json() as Record<string, unknown>;
      expect(afterReopen.pending_approval).toBe(true);

      await approveBtn.click({ force: true });
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true);
    } catch (err) {
      bodyError = err;
    }

    // This click recorded the mutated content's hash, so the restore below
    // re-pends the task — which is why this test runs last in the file and
    // settles explicitly rather than through withPendingChange.
    let cleanupError: unknown;
    try {
      fs.writeFileSync(taskJsPath, original, 'utf8');
      await settleToRestored(request);
    } catch (err) {
      cleanupError = err;
    }
    if (bodyError) throw bodyError;
    if (cleanupError) throw cleanupError;
  });
});
