/**
 * taskset-runtime-alias.spec.ts
 *
 * A taskset entry whose `overrides.runtime` is the legacy `js` alias must
 * resolve to the deno runtime, so the task actually executes.
 */

import { test, expect } from '@playwright/test';

const WEBHOOK_PATH = '/hooks/runtime-alias-override';
const TASK_ID = 'e2e-tests/runtime-alias-override';

test.describe('TaskSet runtime alias override', () => {
  test('overrides.runtime: js runs the task on deno', async ({ request }) => {
    const res = await request.post(WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: { probe: 'runtime-alias' },
    });
    expect(res.ok()).toBe(true);
    const runId = res.headers()['x-run-id'];
    expect(runId).toBeTruthy();

    const body = await res.json() as { ok?: boolean; received?: { probe?: string } };
    expect(body.ok).toBe(true);
    expect(body.received?.probe).toBe('runtime-alias');

    const runRes = await request.get(`/api/runs/${runId}`);
    expect(runRes.ok()).toBe(true);
    const run = await runRes.json() as { task_id?: string; TaskID?: string; status?: string; Status?: string };
    expect(run.task_id || run.TaskID).toBe(TASK_ID);
    expect(run.status || run.Status).toBe('success');
  });

  test('task resolves with runtime deno', async ({ request }) => {
    const res = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}`);
    expect(res.ok()).toBe(true);
    const spec = await res.json() as { runtime?: string; Runtime?: string };
    expect(spec.runtime || spec.Runtime).toBe('deno');
  });
});
