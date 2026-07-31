/**
 * approval-hash-binding.spec.ts
 *
 * E2E coverage for #645: POST /api/tasks/{id}/approve must bind to the exact
 * pending hash the operator's diff was built from, not whatever happens to
 * be pending when the request lands.
 *
 * Before this fix, apiApproveTask called the gate's unconditional Approve()
 * regardless of what (if anything) the caller sent, so a dashboard operator
 * reviewing diff v1 who clicks Approve after the reconciler has already
 * re-pended the task at v2 would silently arm v2 — content they never saw.
 * This suite reproduces that race directly: load the diff, mutate the file
 * again behind the open panel (which never auto-refetches), then click
 * Approve. The stale click must be rejected and the panel must refresh to
 * the version that is actually pending, never silently approve it.
 *
 * Reuses the file-change.spec.ts / approval-diff.spec.ts pattern of mutating
 * the hello-manual fixture in the temp copy at DICODE_E2E_TASKS_DIR and
 * polling the API for the reconciler to catch up. Runs in the same
 * "unauthenticated" project (workers: 1, fullyParallel: false) as
 * approval-diff.spec.ts, so it follows the same non-parallel, restore-exact-
 * bytes-when-done discipline that suite documents.
 */

import { test, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { gotoWebui, navigateInSpa, waitForTaskDetail } from './helpers/webui';

const MANUAL_TASK_ID = 'e2e-tests/hello-manual';

// Worst case per test: ~60s first-pend wait + 60s re-pend wait + 30s cleanup
// convergence loop (plus the API test's own 15s bootstrap-window wait). Give
// real headroom above that sum so a slow CI run reports the helper's own
// timeout error rather than Playwright aborting the test first.
test.setTimeout(180_000);

function tasksDir(): string {
  const d = process.env.DICODE_E2E_TASKS_DIR;
  if (!d) throw new Error('DICODE_E2E_TASKS_DIR not set — global setup may have failed');
  return d;
}

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
 * Polls GET /pending-diff (the source of pending_hash — /api/tasks/{id}
 * itself carries no content-hash field) until the gate reports a pending
 * hash other than notHash, i.e. the reconciler has re-pended the task at a
 * newer version. Returns that new hash. Tracks the last non-matching
 * response so a persistent 409 ("task not pending" — e.g. the write got
 * auto-armed instead of held pending) is distinguishable in the timeout
 * error from a merely slow reconcile.
 */
async function waitForNewPendingHash(
  request: import('@playwright/test').APIRequestContext,
  taskID: string,
  notHash: string,
  timeoutMs = 60_000,
): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  let last = '<no response yet>';
  while (Date.now() < deadline) {
    const res = await request.get(`/api/tasks/${encodeURIComponent(taskID)}/pending-diff`);
    if (res.ok()) {
      const body = await res.json() as { pending_hash?: string };
      if (body.pending_hash && body.pending_hash !== notHash) return body.pending_hash;
      last = `200 pending_hash=${body.pending_hash ?? '<none>'}`;
    } else {
      last = `${res.status()} ${await res.text()}`;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`Task ${taskID} did not re-pend at a new hash within ${timeoutMs}ms (last response: ${last})`);
}

test.describe('Approval hash binding', () => {
  // API-level: the exact race the issue describes, without any UI in the
  // loop. Confirms the server-side contract (409 + stale + task stays
  // pending) that the dashboard behavior below depends on.
  test('approving with a hash the reconciler has since superseded is rejected, not silently applied', async ({ request }) => {
    const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');
    const original = fs.readFileSync(taskJsPath, 'utf8');
    // Markers are unique to this test (not shared with the dashboard-level
    // test below): dicode.lock records one approved hash per task, so if
    // both tests ever produced byte-identical content, a hash this test
    // approves could still be recorded as "approved" when the other test
    // later recreates the same bytes — Gate.Admit's already-approved fast
    // path would then auto-arm it instead of holding it pending, silently
    // defeating the very race this suite exists to reproduce.
    const markerV1 = '// e2e-hash-bind-probe-api-v1';
    const markerV2 = '// e2e-hash-bind-probe-api-v2';

    try {
      // Clear the daemon's 10s approval-bootstrap window (see
      // approval-diff.spec.ts's withPendingChange doc comment) — this is the
      // first mutation in this serial-execution file.
      await new Promise((r) => setTimeout(r, 15_000));

      fs.writeFileSync(taskJsPath, original + `\n${markerV1}\n`, 'utf8');
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval === true);
      const diffRes = await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/pending-diff`);
      expect(diffRes.ok()).toBe(true);
      const staleHash = (await diffRes.json() as { pending_hash: string }).pending_hash;
      expect(staleHash).toBeTruthy();

      // The task re-pends at a newer hash before the (simulated) operator's
      // approve click lands.
      fs.writeFileSync(taskJsPath, original + `\n${markerV1}\n${markerV2}\n`, 'utf8');
      await waitForNewPendingHash(request, MANUAL_TASK_ID, staleHash);

      const approveRes = await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/approve`, {
        data: { hash: staleHash },
      });
      expect(approveRes.status()).toBe(409);
      const approveBody = await approveRes.json() as { stale?: boolean };
      expect(approveBody.stale).toBe(true);

      // Rejected — the task must still be pending, at the newer content.
      const after = await (await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)).json() as Record<string, unknown>;
      expect(after.pending_approval).toBe(true);

      // Approving with the CURRENT hash succeeds.
      const currentDiff = await (await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/pending-diff`)).json() as { pending_hash: string };
      const okRes = await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/approve`, {
        data: { hash: currentDiff.pending_hash },
      });
      expect(okRes.ok()).toBe(true);
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true);
    } finally {
      try {
        fs.writeFileSync(taskJsPath, original, 'utf8');
        const deadline = Date.now() + 30_000;
        for (;;) {
          const t = await (await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)).json() as Record<string, unknown>;
          if (t.pending_approval !== true) break;
          if (Date.now() > deadline) { console.error('task did not settle after restore'); break; }
          await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/approve`);
          await new Promise((r) => setTimeout(r, 1_000));
        }
      } catch (cleanupError) {
        console.error('hash-binding API test cleanup failed:', cleanupError);
      }
    }
  });

  // Dashboard-level: the diff panel is left open on an old version (it never
  // auto-refetches — see dc-task-detail.js's _diff caching), the file
  // changes again underneath it, and the operator clicks the now-stale
  // Approve button. Must refuse, tell the operator, and refresh to the
  // version actually pending — never arm the version they never reviewed.
  test('clicking Approve against a diff the reconciler has superseded refreshes instead of silently approving', async ({ page, request }) => {
    const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');
    const original = fs.readFileSync(taskJsPath, 'utf8');
    // Unique to this test — see the API-level test's comment on why marker
    // uniqueness (not just content difference between v1/v2) matters here.
    const markerV1 = '// e2e-hash-bind-probe-ui-v1';
    const markerV2 = '// e2e-hash-bind-probe-ui-v2';

    try {
      fs.writeFileSync(taskJsPath, original + `\n${markerV1}\n`, 'utf8');
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval === true);

      await gotoWebui(page);
      await navigateInSpa(page, `/tasks/${MANUAL_TASK_ID}`);
      await waitForTaskDetail(page);
      // The diff panel opens automatically for a pending task and shows v1.
      await expect(page.locator('dc-task-detail')).toContainText(markerV1, { timeout: 10_000 });

      const beforeHash = ((await (await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/pending-diff`)).json()) as { pending_hash: string }).pending_hash;

      // Mutate again while the panel is still showing v1 — the loaded _diff
      // is now stale, exactly as it would be after a real reconciler poll
      // landed between the operator opening the panel and clicking Approve.
      fs.writeFileSync(taskJsPath, original + `\n${markerV1}\n${markerV2}\n`, 'utf8');
      await waitForNewPendingHash(request, MANUAL_TASK_ID, beforeHash);

      // The panel must not have refetched on its own — this is the trap the
      // fix closes, not a UI bug to route around.
      await expect(page.locator('dc-task-detail')).toContainText(markerV1);
      await expect(page.locator('dc-task-detail')).not.toContainText(markerV2);

      page.once('dialog', (d) => d.accept());
      await page.locator('button', { hasText: 'Approve' }).click({ force: true });

      // Refused and refreshed: the panel now shows the version that is
      // actually pending, and the task is still awaiting approval.
      await expect(page.locator('dc-task-detail')).toContainText(markerV2, { timeout: 15_000 });
      const stillPending = await (await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)).json() as Record<string, unknown>;
      expect(stillPending.pending_approval).toBe(true);

      // Approving again — now bound to the version actually on screen —
      // succeeds.
      await page.locator('button', { hasText: 'Approve' }).click({ force: true });
      await waitForTaskCondition(request, MANUAL_TASK_ID, (t) => t.pending_approval !== true, 30_000);
    } finally {
      try {
        fs.writeFileSync(taskJsPath, original, 'utf8');
        const deadline = Date.now() + 30_000;
        for (;;) {
          const t = await (await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`)).json() as Record<string, unknown>;
          if (t.pending_approval !== true) break;
          if (Date.now() > deadline) { console.error('task did not settle after restore'); break; }
          await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/approve`);
          await new Promise((r) => setTimeout(r, 1_000));
        }
      } catch (cleanupError) {
        console.error('hash-binding dashboard test cleanup failed:', cleanupError);
      }
    }
  });
});
