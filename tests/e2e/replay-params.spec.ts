/**
 * replay-params.spec.ts
 *
 * Replaying a run restores the fire-time params it ran with. Params the
 * redactor masked at rest (api_token) are not restored: the replay uses the
 * task default and the redaction placeholder never reaches the task.
 *
 * Runs in both the unauthenticated and authenticated projects; the latter
 * logs in first.
 */

import { test, expect, type APIRequestContext } from '@playwright/test';
import { TEST_PASSPHRASE, login } from './helpers/auth';
import { Run, runReturnValue, runStatus } from './helpers/runs';

const TASK_ID = 'e2e-tests/replay-params';

async function waitForTerminal(request: APIRequestContext, runId: string): Promise<Run> {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    const res = await request.get(`/api/runs/${runId}`);
    if (res.ok()) {
      const run = (await res.json()) as Run;
      const status = runStatus(run);
      if (status && status !== 'running') return run;
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`Run ${runId} did not finish`);
}

test.describe('replay restores fire-time params', () => {
  test.setTimeout(90_000);

  test('required param restored, masked param falls back to default', async ({ request }, testInfo) => {
    if (testInfo.project.name === 'authenticated') {
      await login(request, TEST_PASSPHRASE);
    }
    const taskRes = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}`);
    test.skip(!taskRes.ok(), `${TASK_ID} not registered`);

    const fire = await request.post(`/api/tasks/${encodeURIComponent(TASK_ID)}/run`, {
      headers: { 'Content-Type': 'application/json' },
      data: { params: { greeting: 'hello-replay', api_token: 'sk-live-secret' } },
    });
    expect(fire.ok()).toBe(true);
    const { runId } = (await fire.json()) as { runId: string };

    const original = await waitForTerminal(request, runId);
    expect(runStatus(original)).toBe('success');
    expect(JSON.parse(runReturnValue(original))).toEqual({
      greeting: 'hello-replay',
      api_token: 'sk-live-secret',
    });

    // The input blob is persisted asynchronously after the run settles.
    let replayId = '';
    await expect
      .poll(
        async () => {
          const res = await request.post(`/api/runs/${runId}/replay`, {
            headers: { 'Content-Type': 'application/json' },
            data: {},
          });
          if (!res.ok()) return res.status();
          replayId = ((await res.json()) as { run_id: string }).run_id;
          return 200;
        },
        { timeout: 20_000, intervals: [500] },
      )
      .toBe(200);

    const replayed = await waitForTerminal(request, replayId);
    expect(runStatus(replayed), 'required param must satisfy preflight on replay').toBe('success');
    const out = JSON.parse(runReturnValue(replayed)) as Record<string, string>;
    expect(out.greeting).toBe('hello-replay');
    expect(out.api_token).toBe('default-token');
    expect(runReturnValue(replayed)).not.toContain('sk-live-secret');
  });
});
