/**
 * resume-params-redaction.spec.ts
 *
 * End-to-end coverage for #817: a fire-time param whose value is backed by a
 * live secrets-chain entry must never reach GET /api/runs/<id> in the clear
 * while a run is suspended, and the resumed continuation must still receive
 * the REAL value (not the redaction placeholder) so the wizard behaves
 * correctly.
 *
 * Reuses the e2e-tests/suspend-wizard fixture (suspend-resume.spec.ts), which
 * declares an optional `api_key` param and echoes it back via `params.get`
 * on resume.
 */

import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

const TASK_ID = 'e2e-tests/suspend-wizard';
const SECRET_KEY = 'api_key';
const SECRET_VALUE = 'sk_live_e2eRegressionSecret817';

function runStatus(run: Record<string, unknown>): string {
  return (run.Status ?? run.status) as string;
}

async function getRun(request: APIRequestContext, runID: string): Promise<Record<string, unknown>> {
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

test.describe('resume params redaction (#817)', () => {
  test.setTimeout(60_000);

  test('secret-backed param is redacted at rest and restored correctly on resume', async ({ request }) => {
    const taskRes = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}`);
    if (!taskRes.ok()) {
      test.skip(true, `${TASK_ID} not registered — fixture taskset missing it?`);
      return;
    }

    // Seed a secret under the same name as the param we're about to fire with.
    const setSecret = await request.post('/api/secrets', {
      headers: { 'Content-Type': 'application/json' },
      data: { key: SECRET_KEY, value: SECRET_VALUE },
    });
    if (!setSecret.ok()) {
      test.skip(true, 'POST /api/secrets not available — secrets store not configured');
      return;
    }

    // Fire with a param value equal to the live secret — the only case #817
    // redacts.
    const fireRes = await request.post(`/api/tasks/${encodeURIComponent(TASK_ID)}/run`, {
      headers: { 'Content-Type': 'application/json' },
      data: { params: { api_key: SECRET_VALUE } },
    });
    expect(fireRes.ok()).toBe(true);
    const { runId } = (await fireRes.json()) as { runId: string };
    expect(runId).toBeTruthy();

    await waitForStatus(request, runId, 'suspended');

    // The raw API response must never contain the secret value, and must not
    // even carry a ResumeParams field at all (server-internal, like
    // ResumeState/ResumeToken).
    const rawRes = await request.get(`/api/runs/${runId}`);
    expect(rawRes.ok()).toBe(true);
    const rawText = await rawRes.text();
    expect(rawText).not.toContain(SECRET_VALUE);

    const suspended = JSON.parse(rawText) as Record<string, unknown>;
    expect(suspended.ResumeParams ?? null).toBeNull();

    // The metadata says which param was sensitive, without revealing it.
    const redacted = (suspended.ResumeParamsRedactedFields ?? []) as string[];
    expect(redacted).toContain('params.api_key');

    // Resume the wizard — the continuation must get the REAL secret value.
    const resumeRes = await request.post(`/api/runs/${runId}/resume`, {
      headers: { 'Content-Type': 'application/json' },
      data: { project_name: 'redaction-e2e' },
    });
    expect(resumeRes.ok()).toBe(true);
    const { run_id: continuationId } = (await resumeRes.json()) as { run_id: string };
    expect(continuationId).toBeTruthy();

    const done = await waitForStatus(request, continuationId, 'success');
    const returnValue = (done.ReturnValue ?? done.return_value) as string;
    expect(returnValue).toContain('redaction-e2e');

    const parsed = JSON.parse(returnValue) as { created?: string; api_key?: string };
    expect(parsed.api_key).toBe(SECRET_VALUE);
  });

  test('a param with no matching live secret round-trips unredacted', async ({ request }) => {
    const taskRes = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}`);
    if (!taskRes.ok()) {
      test.skip(true, `${TASK_ID} not registered — fixture taskset missing it?`);
      return;
    }

    const literalValue = 'literal-value-not-in-secrets-store';
    const fireRes = await request.post(`/api/tasks/${encodeURIComponent(TASK_ID)}/run`, {
      headers: { 'Content-Type': 'application/json' },
      data: { params: { api_key: literalValue } },
    });
    expect(fireRes.ok()).toBe(true);
    const { runId } = (await fireRes.json()) as { runId: string };

    const suspended = await waitForStatus(request, runId, 'suspended');
    const redacted = (suspended.ResumeParamsRedactedFields ?? []) as string[];
    expect(redacted).not.toContain('params.api_key');

    const resumeRes = await request.post(`/api/runs/${runId}/resume`, {
      headers: { 'Content-Type': 'application/json' },
      data: { project_name: 'literal-e2e' },
    });
    expect(resumeRes.ok()).toBe(true);
    const { run_id: continuationId } = (await resumeRes.json()) as { run_id: string };

    const done = await waitForStatus(request, continuationId, 'success');
    const parsed = JSON.parse((done.ReturnValue ?? done.return_value) as string) as { api_key?: string };
    expect(parsed.api_key).toBe(literalValue);
  });
});
