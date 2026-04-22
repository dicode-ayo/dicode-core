/**
 * webhooks.spec.ts
 *
 * Tests for the open (no HMAC) webhook endpoint:
 * - POST /hooks/test-webhook fires the task and returns result
 * - X-Run-Id header present in response
 * - Run completes successfully with correct output
 * - Async mode (?wait=false) returns runId immediately
 * - Webhooks bypass auth wall (tested in unauthenticated project but same
 *   behaviour applies when auth is enabled — see auth.spec.ts for that)
 */

import { test, expect } from '@playwright/test';
import { gotoWebui, navigateInSpa, waitForRunDetail } from './helpers/webui';

const WEBHOOK_PATH = '/hooks/test-webhook';
const WEBHOOK_TASK_ID = 'e2e-tests/hello-webhook';

test.describe('Open Webhook', () => {
  test('POST to webhook returns 200 with JSON body', async ({ request }) => {
    const payload = { hello: 'world', value: 42 };
    const res = await request.post(WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: payload,
    });
    expect(res.ok()).toBe(true);
  });

  test('POST sets X-Run-Id response header', async ({ request }) => {
    const res = await request.post(WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: { test: 'x-run-id' },
    });
    const runId = res.headers()['x-run-id'];
    expect(runId).toBeTruthy();
    expect(runId.length).toBeGreaterThan(0);
  });

  test('webhook run result contains input payload', async ({ request }) => {
    const payload = { source: 'e2e-test', ts: Date.now() };
    const res = await request.post(WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: payload,
    });
    expect(res.ok()).toBe(true);

    // The task returns { received: input } — should be embedded in the response body.
    const body = await res.json() as Record<string, unknown>;
    // The return value is serialized into the response directly.
    expect(body).toHaveProperty('received');
    const received = body.received as Record<string, unknown>;
    expect(received.source).toBe('e2e-test');
  });

  test('POST with ?wait=false returns runId immediately', async ({ request }) => {
    const res = await request.post(`${WEBHOOK_PATH}?wait=false`, {
      headers: { 'Content-Type': 'application/json' },
      data: { async: true },
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as { runId: string };
    expect(body.runId).toBeTruthy();

    // X-Run-Id header also set in async mode.
    const runIdHeader = res.headers()['x-run-id'];
    expect(runIdHeader).toBe(body.runId);
  });

  test('run triggered by webhook appears in /api/runs', async ({ request }) => {
    const res = await request.post(WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: { trace: 'e2e' },
    });
    const runId = res.headers()['x-run-id'];
    expect(runId).toBeTruthy();

    const runRes = await request.get(`/api/runs/${runId}`);
    expect(runRes.ok()).toBe(true);
    const run = await runRes.json() as { task_id?: string; TaskID?: string; status?: string; Status?: string };
    expect(run.task_id || run.TaskID).toBe(WEBHOOK_TASK_ID);
    const status = run.status || run.Status;
    expect(['success', 'failure', 'running']).toContain(status);
  });

  test('webhook run navigable in UI', async ({ page, request }) => {
    // Fire and wait for the async run to complete.
    const res = await request.post(`${WEBHOOK_PATH}?wait=false`, {
      headers: { 'Content-Type': 'application/json' },
      data: { ui_test: true },
    });
    const runId = res.headers()['x-run-id'];
    expect(runId).toBeTruthy();

    // Poll until complete.
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const r = await request.get(`/api/runs/${runId}`);
      if (r.ok()) {
        const b = await r.json() as { status?: string; Status?: string };
        const s = b.status || b.Status;
        if (s && s !== 'running') break;
      }
      await new Promise((r2) => setTimeout(r2, 500));
    }

    // Navigate to run detail via the SPA.
    await gotoWebui(page);
    await navigateInSpa(page, `/runs/${runId}`);
    await waitForRunDetail(page);

    await expect(page.locator('.badge-success')).toBeVisible();
  });

  test('GET to webhook without index.html fires task (falls through to run)', async ({ request }) => {
    // Without an index.html in the task dir, GET also triggers the task.
    const res = await request.get(`${WEBHOOK_PATH}?param=value`);
    // Should return 200 (synchronous run result) or at least not 404.
    expect(res.status()).not.toBe(404);
  });

  test('webhook logs contain received input', async ({ request }) => {
    const payload = { log_check: 'yes', value: 999 };
    const res = await request.post(WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: payload,
    });
    const runId = res.headers()['x-run-id'];
    expect(runId).toBeTruthy();

    const logsRes = await request.get(`/api/runs/${runId}/logs`);
    expect(logsRes.ok()).toBe(true);
    const logs = await logsRes.json() as Array<{ message: string }>;
    const allMessages = logs.map((l) => l.message).join('\n');
    expect(allMessages).toContain('webhook received');
  });
});
