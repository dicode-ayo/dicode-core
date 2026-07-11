/**
 * run-tree-collapse.spec.ts
 *
 * E2E coverage for #573: a run's descendant tree collapses under its root
 * run. A suspend/resume conversation (original run + its resume
 * continuation) shares one root_run_id (#569); the WebUI run list shows it
 * as a single top-level row, and the expand toggle (`▸`/`▾` →
 * GET /api/runs?root=<id>` → `_renderDescendants`) reveals the
 * continuation nested underneath.
 */

import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';
import { gotoWebui, navigateInSpa, waitForTaskDetail } from './helpers/webui';

const TASK_ID = 'e2e-tests/suspend-wizard';

function runStatus(run: Record<string, unknown>): string {
  return (run.Status ?? run.status) as string;
}

function rootRunID(run: Record<string, unknown>): string {
  return (run.RootRunID ?? run.root_run_id) as string;
}

function runID(run: Record<string, unknown>): string {
  return (run.ID ?? run.id) as string;
}

async function getRun(
  request: APIRequestContext,
  id: string,
): Promise<Record<string, unknown>> {
  const res = await request.get(`/api/runs/${id}`);
  expect(res.ok()).toBe(true);
  return (await res.json()) as Record<string, unknown>;
}

async function waitForStatus(
  request: APIRequestContext,
  id: string,
  want: string,
  timeoutMs = 30_000,
): Promise<Record<string, unknown>> {
  const deadline = Date.now() + timeoutMs;
  let last = '';
  while (Date.now() < deadline) {
    const run = await getRun(request, id);
    last = runStatus(run);
    if (last === want) return run;
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`Run ${id} did not reach ${want} within ${timeoutMs}ms (last=${last})`);
}

/** Fire the suspend-wizard, resume it, and wait for the continuation to finish. */
async function spawnConversation(
  request: APIRequestContext,
): Promise<{ rootId: string; continuationId: string }> {
  const fireRes = await request.post(
    `/api/tasks/${encodeURIComponent(TASK_ID)}/run`,
    { headers: { 'Content-Type': 'application/json' } },
  );
  expect(fireRes.ok()).toBe(true);
  const { runId } = (await fireRes.json()) as { runId: string };
  await waitForStatus(request, runId, 'suspended');

  const resumeRes = await request.post(`/api/runs/${runId}/resume`, {
    headers: { 'Content-Type': 'application/json' },
    data: { project_name: 'run-tree-collapse-e2e' },
  });
  expect(resumeRes.ok()).toBe(true);
  const { run_id: continuationId } = (await resumeRes.json()) as { run_id: string };
  await waitForStatus(request, continuationId, 'success');

  return { rootId: runId, continuationId };
}

test.describe('run-tree collapse (#573)', () => {
  test.setTimeout(60_000);

  test('root and continuation share a root_run_id, queryable via /api/runs?root=', async ({ request }) => {
    const taskRes = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}`);
    if (!taskRes.ok()) {
      test.skip(true, `${TASK_ID} not registered — fixture taskset missing it?`);
      return;
    }

    const { rootId, continuationId } = await spawnConversation(request);

    const root = await getRun(request, rootId);
    expect(rootRunID(root)).toBe(rootId);

    const continuation = await getRun(request, continuationId);
    expect(rootRunID(continuation)).toBe(rootId);

    const groupRes = await request.get(`/api/runs?root=${rootId}`);
    expect(groupRes.ok()).toBe(true);
    const group = (await groupRes.json()) as Array<Record<string, unknown>>;
    const ids = group.map(runID);
    expect(ids).toContain(rootId);
    expect(ids).toContain(continuationId);
  });

  test('the conversation renders as one row that expands to reveal the continuation', async ({ page, request }) => {
    const taskRes = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}`);
    if (!taskRes.ok()) {
      test.skip(true, `${TASK_ID} not registered — fixture taskset missing it?`);
      return;
    }

    const { rootId, continuationId } = await spawnConversation(request);

    await gotoWebui(page);
    // Client-side navigate (not page.goto) — the task ID contains a "/" and
    // the SPA router matches the literal pushState path, not a URL-decoded
    // one, so a full page load of an encoded URL would hand the component a
    // still-percent-encoded taskid.
    await navigateInSpa(page, '/tasks/' + TASK_ID);
    await waitForTaskDetail(page);

    // The conversation is exactly one top-level row (filtered by
    // RootRunID === ID), not two flat rows.
    const rootLink = page.locator(`a[href="runs/${rootId}"]`);
    await expect(rootLink).toBeVisible({ timeout: 15_000 });
    await expect(rootLink).toHaveCount(1);
    await expect(rootLink).toContainText(rootId.slice(0, 8));

    // The continuation is folded under the root, not rendered as its own row.
    const continuationLink = page.locator(`a[href="runs/${continuationId}"]`);
    await expect(continuationLink).toHaveCount(0);

    const rootRow = page.locator('tr', { has: rootLink });
    const expandBtn = rootRow.locator('button[title="Show descendant runs"]');
    await expect(expandBtn).toBeVisible();
    await expandBtn.click();

    // Expanding reveals the continuation nested underneath.
    await expect(continuationLink).toBeVisible({ timeout: 10_000 });
    await expect(continuationLink).toContainText(continuationId.slice(0, 8));
  });
});
