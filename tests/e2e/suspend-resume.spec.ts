/**
 * suspend-resume.spec.ts
 *
 * End-to-end coverage for the suspend/resume WebUI surface (#95, PR 4):
 * fire a task that calls dicode.suspend(), confirm the run lands in
 * `suspended` with a form, submit the form via POST /api/runs/<id>/resume,
 * and assert the continuation run succeeds with the submitted value while the
 * original run transitions to `resumed`.
 */

import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

const TASK_ID = 'e2e-tests/suspend-wizard';

function runStatus(run: Record<string, unknown>): string {
  return (run.Status ?? run.status) as string;
}

async function getRun(
  request: APIRequestContext,
  runID: string,
): Promise<Record<string, unknown>> {
  const res = await request.get(`/api/runs/${runID}`);
  expect(res.ok()).toBe(true);
  return (await res.json()) as Record<string, unknown>;
}

async function waitForStatus(
  request: APIRequestContext,
  runID: string,
  want: string,
  timeoutMs = 30_000,
): Promise<Record<string, unknown>> {
  const deadline = Date.now() + timeoutMs;
  let last = '';
  while (Date.now() < deadline) {
    const run = await getRun(request, runID);
    last = runStatus(run);
    if (last === want) return run;
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`Run ${runID} did not reach ${want} within ${timeoutMs}ms (last=${last})`);
}

test.describe('suspend/resume webui', () => {
  test.setTimeout(60_000);

  test('suspend → fill form → resume spawns the continuation', async ({ request }) => {
    const taskRes = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}`);
    if (!taskRes.ok()) {
      test.skip(true, `${TASK_ID} not registered — fixture taskset missing it?`);
      return;
    }

    // Fire the wizard — first invocation suspends.
    const fireRes = await request.post(
      `/api/tasks/${encodeURIComponent(TASK_ID)}/run`,
      { headers: { 'Content-Type': 'application/json' } },
    );
    expect(fireRes.ok()).toBe(true);
    const { runId } = (await fireRes.json()) as { runId: string };
    expect(runId).toBeTruthy();

    // It must land suspended, carrying a decoded form — and NOT leak a token.
    const suspended = await waitForStatus(request, runId, 'suspended');
    const form = suspended.resume_form as { fields?: { name: string }[] } | undefined;
    expect(form?.fields?.some((f) => f.name === 'project_name')).toBe(true);
    // The token is the resume authorization — it must never reach the client.
    expect(suspended.ResumeToken ?? '').toBe('');
    expect(suspended.ResumeState ?? null).toBeNull();

    // Missing required field → 400.
    const bad = await request.post(`/api/runs/${runId}/resume`, {
      headers: { 'Content-Type': 'application/json' },
      data: {},
    });
    expect(bad.status()).toBe(400);

    // Submit the form → continuation run id.
    const ok = await request.post(`/api/runs/${runId}/resume`, {
      headers: { 'Content-Type': 'application/json' },
      data: { project_name: 'webui-e2e' },
    });
    expect(ok.ok()).toBe(true);
    const { run_id: continuationId } = (await ok.json()) as { run_id: string };
    expect(continuationId).toBeTruthy();
    expect(continuationId).not.toBe(runId);

    // Continuation succeeds with the submitted value echoed back.
    const done = await waitForStatus(request, continuationId, 'success');
    expect((done.ReturnValue ?? done.return_value) as string).toContain('webui-e2e');
    expect((done.ParentRunID ?? done.parent_run_id) as string).toBe(runId);

    // Original run is now resumed; a second submit is rejected (single-use).
    const original = await getRun(request, runId);
    expect(runStatus(original)).toBe('resumed');

    const replay = await request.post(`/api/runs/${runId}/resume`, {
      headers: { 'Content-Type': 'application/json' },
      data: { project_name: 'again' },
    });
    expect(replay.status()).toBe(409);
  });
});
