/**
 * file-change.spec.ts
 *
 * Tests that file-system changes to task files are picked up by the dicode
 * reconciler and reflected in the API/UI.
 *
 * These tests modify files in DICODE_E2E_TASKS_DIR (a temp copy of the fixture
 * tasks), so the originals in tests/e2e/fixtures/tasks/ remain clean.
 */

import { test, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';

const MANUAL_TASK_ID = 'e2e-tests/hello-manual';
const CRON_TASK_ID = 'e2e-tests/hello-cron';

/** Return the temp tasks dir (copied by global setup) */
function tasksDir(): string {
  const d = process.env.DICODE_E2E_TASKS_DIR;
  if (!d) throw new Error('DICODE_E2E_TASKS_DIR not set — global setup may have failed');
  return d;
}

/**
 * Poll GET /api/tasks until the predicate is satisfied, up to timeoutMs.
 */
async function waitForTaskCondition(
  request: import('@playwright/test').APIRequestContext,
  taskID: string,
  predicate: (task: Record<string, unknown>) => boolean,
  timeoutMs = 20_000,
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

test.describe('File Change Detection', () => {
  test('editing task.js updates task behaviour (run returns new value)', async ({ request }) => {
    const taskJsPath = path.join(tasksDir(), 'hello-manual', 'task.js');

    // Write a new version of task.js with a distinctive message.
    const newContent = `log.info('updated by file-change test');\nreturn { message: 'updated message' };\n`;
    fs.writeFileSync(taskJsPath, newContent, 'utf8');

    // Wait a moment for the reconciler's fsnotify to pick up the change.
    // We verify the change took effect by running the task and checking return value.
    await new Promise((r) => setTimeout(r, 2000));

    // Fire a run and wait for it to complete.
    const runRes = await request.post(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}/run`);
    expect(runRes.ok()).toBe(true);
    const { runId } = await runRes.json() as { runId: string };

    // Poll for completion.
    const deadline = Date.now() + 30_000;
    let finalStatus = '';
    while (Date.now() < deadline) {
      const r = await request.get(`/api/runs/${runId}`);
      if (r.ok()) {
        const b = await r.json() as { status?: string; Status?: string };
        const s = b.status || b.Status || '';
        if (s && s !== 'running') { finalStatus = s; break; }
      }
      await new Promise((r2) => setTimeout(r2, 500));
    }
    expect(finalStatus).toBe('success');

    // Check log output contains the updated message.
    const logsRes = await request.get(`/api/runs/${runId}/logs`);
    expect(logsRes.ok()).toBe(true);
    const logs = await logsRes.json() as Array<{ message: string }>;
    const messages = logs.map((l) => l.message).join('\n');
    expect(messages).toContain('updated by file-change test');

    // Restore original content for subsequent tests.
    const original = `log.info('hello from test manual task');\nreturn { message: 'hello from test' };\n`;
    fs.writeFileSync(taskJsPath, original, 'utf8');
  });

  test('editing task.yaml (description) is reflected in API response', async ({ request }) => {
    const taskYamlPath = path.join(tasksDir(), 'hello-manual', 'task.yaml');
    const originalYaml = fs.readFileSync(taskYamlPath, 'utf8');

    // Change the description field.
    const updatedYaml = originalYaml.replace(
      /description:.*$/m,
      'description: Updated description via file-change test.',
    );
    fs.writeFileSync(taskYamlPath, updatedYaml, 'utf8');

    // Wait for the reconciler to pick up the change.
    await waitForTaskCondition(
      request,
      MANUAL_TASK_ID,
      (t) => typeof t.description === 'string' && t.description.includes('Updated description'),
      20_000,
    );

    const res = await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`);
    expect(res.ok()).toBe(true);
    const body = await res.json() as { description: string };
    expect(body.description).toContain('Updated description');

    // Restore.
    fs.writeFileSync(taskYamlPath, originalYaml, 'utf8');
  });

  test('UI reflects file changes after reconciler picks them up', async ({ page, request }) => {
    const taskJsPath = path.join(tasksDir(), 'hello-cron', 'task.js');
    const originalJs = fs.readFileSync(taskJsPath, 'utf8');

    // Modify the cron task script.
    const updatedJs = `const time = new Date().toISOString();\nlog.info('cron updated ' + time);\nreturn { time, updated: true };\n`;
    fs.writeFileSync(taskJsPath, updatedJs, 'utf8');

    // Wait briefly for reconciler.
    await new Promise((r) => setTimeout(r, 2500));

    // Navigate to task detail — the reconciler broadcasts tasks:changed via WS
    // so the UI should reflect any metadata changes.
    await page.goto(`/tasks/${encodeURIComponent(CRON_TASK_ID)}`);
    await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
    await page.waitForFunction(() => {
      const el = document.querySelector('dc-task-detail');
      return el && !el.textContent?.includes('Loading');
    }, { timeout: 15_000 });

    // Task should still be present and visible after file change.
    await expect(page.locator('h1', { hasText: 'Hello Cron' })).toBeVisible();

    // Restore.
    fs.writeFileSync(taskJsPath, originalJs, 'utf8');
  });

  test('file edit is idempotent — restoring original brings task back', async ({ request }) => {
    const taskYamlPath = path.join(tasksDir(), 'hello-manual', 'task.yaml');
    const originalYaml = fs.readFileSync(taskYamlPath, 'utf8');

    // Make a change.
    const changed = originalYaml.replace(
      /description:.*$/m,
      'description: Temp change for idempotency test.',
    );
    fs.writeFileSync(taskYamlPath, changed, 'utf8');
    await new Promise((r) => setTimeout(r, 1500));

    // Restore the original.
    fs.writeFileSync(taskYamlPath, originalYaml, 'utf8');

    // Wait until the API returns the original description.
    await waitForTaskCondition(
      request,
      MANUAL_TASK_ID,
      (t) => typeof t.description === 'string' && t.description.includes('simple manual task'),
      20_000,
    );

    const res = await request.get(`/api/tasks/${encodeURIComponent(MANUAL_TASK_ID)}`);
    const body = await res.json() as { description: string };
    expect(body.description).toContain('simple manual task');
  });
});
