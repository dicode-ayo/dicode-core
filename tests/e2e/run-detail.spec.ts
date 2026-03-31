/**
 * run-detail.spec.ts
 *
 * Tests for the run detail page (dc-run-detail component):
 * - Trigger a manual run via API
 * - Navigate to /runs/{runID}
 * - Status badge and log lines appear
 * - Run eventually transitions to success
 * - Final status shown correctly
 */

import { test, expect } from '@playwright/test';

const MANUAL_TASK_ID = 'e2e-tests/hello-manual';

/** Fire a run via API and return the runId */
async function triggerRun(request: import('@playwright/test').APIRequestContext): Promise<string> {
  const res = await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/run`);
  expect(res.ok()).toBe(true);
  const body = await res.json();
  expect(body.runId).toBeTruthy();
  return body.runId as string;
}

/** Poll GET /api/runs/{id} until status is not 'running', up to 30s */
async function waitForCompletion(
  request: import('@playwright/test').APIRequestContext,
  runID: string,
  timeoutMs = 30_000,
): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await request.get(`/api/runs/${runID}`);
    if (res.ok()) {
      const body = await res.json();
      const status: string = body.status || body.Status;
      if (status && status !== 'running') return status;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`Run ${runID} did not complete within ${timeoutMs}ms`);
}

test.describe('Run Detail', () => {
  test('API: fire run returns runId', async ({ request }) => {
    const runID = await triggerRun(request);
    expect(typeof runID).toBe('string');
    expect(runID.length).toBeGreaterThan(0);
  });

  test('API: run status endpoint returns valid shape', async ({ request }) => {
    const runID = await triggerRun(request);
    const res = await request.get(`/api/runs/${runID}`);
    expect(res.ok()).toBe(true);
    const body = await res.json();
    expect(body).toHaveProperty('task_id', MANUAL_TASK_ID);
    expect(['running', 'success', 'failure']).toContain(body.status || body.Status);
  });

  test('API: run completes with success', async ({ request }) => {
    const runID = await triggerRun(request);
    const finalStatus = await waitForCompletion(request, runID);
    expect(finalStatus).toBe('success');
  });

  test('API: logs endpoint returns log entries', async ({ request }) => {
    const runID = await triggerRun(request);
    await waitForCompletion(request, runID);
    const res = await request.get(`/api/runs/${runID}/logs`);
    expect(res.ok()).toBe(true);
    const logs = await res.json();
    expect(Array.isArray(logs)).toBe(true);
    expect(logs.length).toBeGreaterThan(0);
    // Each entry should have level, message, ts
    expect(logs[0]).toHaveProperty('level');
    expect(logs[0]).toHaveProperty('message');
    expect(logs[0]).toHaveProperty('ts');
  });

  test('UI: run detail page shows status badge', async ({ page, request }) => {
    const runID = await triggerRun(request);
    await page.goto(`/runs/${runID}`);
    await page.waitForSelector('dc-run-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-run-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });

    // Status badge should be visible
    await expect(page.locator('.badge').first()).toBeVisible();
  });

  test('UI: run detail shows log output section', async ({ page, request }) => {
    const runID = await triggerRun(request);
    // Wait for completion so logs are fully written
    await waitForCompletion(request, runID);

    await page.goto(`/runs/${runID}`);
    await page.waitForSelector('dc-run-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-run-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });

    await expect(page.locator('h2', { hasText: 'Logs' })).toBeVisible();
    await expect(page.locator('#log-output')).toBeVisible();
  });

  test('UI: run detail shows task name link back to task', async ({ page, request }) => {
    const runID = await triggerRun(request);
    await waitForCompletion(request, runID);

    await page.goto(`/runs/${runID}`);
    await page.waitForSelector('dc-run-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-run-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });

    // Back link "← Hello Manual"
    await expect(page.locator('a', { hasText: 'Hello Manual' })).toBeVisible();
  });

  test('UI: completed run shows success badge', async ({ page, request }) => {
    const runID = await triggerRun(request);
    await waitForCompletion(request, runID);

    await page.goto(`/runs/${runID}`);
    await page.waitForSelector('dc-run-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-run-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });

    await expect(page.locator('.badge-success')).toBeVisible();
  });

  test('UI: log lines appear in the pre element', async ({ page, request }) => {
    const runID = await triggerRun(request);
    await waitForCompletion(request, runID);

    await page.goto(`/runs/${runID}`);
    await page.waitForSelector('#log-output', { timeout: 15_000 });
    const logText = await page.locator('#log-output').textContent();
    // Task logs "hello from test manual task"
    expect(logText).toContain('hello from test manual task');
  });
});
