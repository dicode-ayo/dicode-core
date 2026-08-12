/**
 * task-load-failure.spec.ts
 *
 * E2E regression coverage for #649: a task.yaml that fails to parse used to
 * vanish from the UI entirely — the task list's count dropped, the source
 * dot stayed green, and daemon.log was the only place that recorded a
 * failure. This reproduces the reported repro end-to-end (mutate a fixture
 * task.yaml into something that fails to unmarshal, watch the task
 * disappear-or-not from the dashboard) against a dedicated fixture task,
 * `load-error-target`, so this spec's mutation can't collide with any other
 * spec's use of the shared hello-manual/hello-cron fixtures.
 *
 * Before the fix: the taskset resolver silently dropped the failed entry
 * from its result set, which made pkg/source.DiffSnapshots see it as REMOVED
 * — the reconciler genuinely unregistered it, and both the task list count
 * and the /api/tasks response lost the entry. This spec would have failed on
 * that behavior (count drop + entry absent).
 *
 * After the fix: the entry stays registered (or, for a first-time failure,
 * visible via a synthetic row) with a `load_error` field, and GET
 * /api/sources reports a non-zero failed_count for the "e2e-tests" source.
 */

import { test, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { gotoWebui, navigateInSpa } from './helpers/webui';

const TASK_ID = 'e2e-tests/load-error-target';
const SOURCE_NAME = 'e2e-tests';

function tasksDir(): string {
  const d = process.env.DICODE_E2E_TASKS_DIR;
  if (!d) throw new Error('DICODE_E2E_TASKS_DIR not set — global setup may have failed');
  return d;
}

type TaskListEntry = { id: string; load_error?: string };

/** Fetch GET /api/tasks and return the parsed JSON array. */
async function listTasks(request: import('@playwright/test').APIRequestContext): Promise<TaskListEntry[]> {
  const res = await request.get('/api/tasks');
  expect(res.ok()).toBe(true);
  return (await res.json()) as TaskListEntry[];
}

type SourceEntry = {
  name: string;
  failed_count?: number;
  failures?: Array<{ id: string; error: string }>;
};

async function getSource(
  request: import('@playwright/test').APIRequestContext,
  name: string,
): Promise<SourceEntry | undefined> {
  const res = await request.get('/api/sources');
  expect(res.ok()).toBe(true);
  const sources = (await res.json()) as SourceEntry[];
  return sources.find((s) => s.name === name);
}

/** Poll until predicate(tasks) is true, up to timeoutMs. */
async function waitForTasksCondition(
  request: import('@playwright/test').APIRequestContext,
  predicate: (tasks: TaskListEntry[]) => boolean,
  timeoutMs = 20_000,
): Promise<TaskListEntry[]> {
  const deadline = Date.now() + timeoutMs;
  let last: TaskListEntry[] = [];
  while (Date.now() < deadline) {
    last = await listTasks(request);
    if (predicate(last)) return last;
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`GET /api/tasks did not satisfy condition within ${timeoutMs}ms; last=${JSON.stringify(last)}`);
}

test.describe('Task load failure surfacing (#649)', () => {
  test('a task.yaml that fails to parse stays in the list with a load error, and the source reports a failed count', async ({
    page,
    request,
  }) => {
    const taskYamlPath = path.join(tasksDir(), 'load-error-target', 'task.yaml');
    const original = fs.readFileSync(taskYamlPath, 'utf8');

    try {
      // Baseline: the task is registered cleanly and carries no load_error.
      const before = await waitForTasksCondition(request, (tasks) =>
        tasks.some((t) => t.id === TASK_ID));
      const beforeCount = before.length;
      const beforeEntry = before.find((t) => t.id === TASK_ID)!;
      expect(beforeEntry.load_error ?? '').toBe('');

      const sourceBefore = await getSource(request, SOURCE_NAME);
      expect(sourceBefore?.failed_count ?? 0).toBe(0);

      // Break it the same way #649's daemon.log line reproduced: a []string
      // field (hash_include) gets a bare bool.
      const broken =
        'apiVersion: dicode/v1\nkind: Task\nname: Load Error Target\nruntime: deno\n' +
        'trigger:\n  manual: true\nhash_include: true\n';
      fs.writeFileSync(taskYamlPath, broken, 'utf8');

      // The task must NOT disappear: same total count, and the row itself is
      // still present — now flagged with a load_error. This is the exact
      // assertion that would have failed before the fix (the entry would
      // have dropped out of the list entirely, shrinking the count by 1).
      const after = await waitForTasksCondition(request, (tasks) => {
        const entry = tasks.find((t) => t.id === TASK_ID);
        return !!entry && !!entry.load_error;
      });
      expect(after.length).toBe(beforeCount);
      const afterEntry = after.find((t) => t.id === TASK_ID)!;
      expect(afterEntry.load_error).toContain('unmarshal');

      // The Sources page's API must reflect a failed entry for this source,
      // and the failure detail must name the broken task.
      const sourceAfter = await getSource(request, SOURCE_NAME);
      expect(sourceAfter?.failed_count ?? 0).toBeGreaterThan(0);
      expect((sourceAfter?.failures ?? []).some((f) => f.id === TASK_ID)).toBe(true);

      // Drive the dashboard: the task row is visible (not vanished) with a
      // "load error" badge, and the Sources page shows the failed count
      // rather than an all-clear state.
      await gotoWebui(page);
      const row = page.locator(`dc-task-list tr[data-task-id="${TASK_ID}"]`);
      await expect(row).toBeVisible();
      await expect(row.locator('.badge-failure', { hasText: 'load error' })).toBeVisible();

      await navigateInSpa(page, '/sources');
      await page.waitForSelector('dc-sources', { timeout: 10_000 });
      await expect(page.locator('dc-sources')).toContainText('failed to load', { timeout: 10_000 });
    } finally {
      // Restore exact original bytes and wait for the load_error to clear —
      // errors here are logged rather than thrown so a genuine assertion
      // failure above isn't masked by a cleanup problem.
      try {
        fs.writeFileSync(taskYamlPath, original, 'utf8');
        await waitForTasksCondition(request, (tasks) => {
          const entry = tasks.find((t) => t.id === TASK_ID);
          return !!entry && !entry.load_error;
        });
      } catch (cleanupError) {
        console.error('task-load-failure cleanup failed:', cleanupError);
      }
    }
  });

  // A second, different-shaped break (still broken, but different bytes so
  // the dir hash changes again) must not create a duplicate row or drop the
  // count either — the failure record replaces itself, it doesn't stack.
  test('repeated edits while broken do not duplicate or drop the row', async ({ request }) => {
    const taskYamlPath = path.join(tasksDir(), 'load-error-target', 'task.yaml');
    const original = fs.readFileSync(taskYamlPath, 'utf8');

    try {
      const before = await waitForTasksCondition(request, (tasks) => {
        const entry = tasks.find((t) => t.id === TASK_ID);
        return !!entry && !entry.load_error;
      });
      const baselineCount = before.length;

      const broken1 =
        'apiVersion: dicode/v1\nkind: Task\nname: Load Error Target\nruntime: deno\n' +
        'trigger:\n  manual: true\nhash_include: true\n';
      fs.writeFileSync(taskYamlPath, broken1, 'utf8');
      await waitForTasksCondition(request, (tasks) => {
        const entry = tasks.find((t) => t.id === TASK_ID);
        return !!entry && !!entry.load_error;
      });

      // Edit again while still broken — same invalid field, but with a
      // trailing comment so the bytes (and thus the dir hash) actually
      // change, forcing a second resolve pass over the still-broken entry.
      const broken2 = broken1 + '# still broken, re-synced\n';
      fs.writeFileSync(taskYamlPath, broken2, 'utf8');
      await new Promise((r) => setTimeout(r, 2_000));

      const tasks = await listTasks(request);
      const matches = tasks.filter((t) => t.id === TASK_ID);
      expect(matches.length).toBe(1);
      expect(matches[0].load_error).toBeTruthy();
      expect(tasks.length).toBe(baselineCount);
    } finally {
      try {
        fs.writeFileSync(taskYamlPath, original, 'utf8');
        await waitForTasksCondition(request, (tasks) => {
          const entry = tasks.find((t) => t.id === TASK_ID);
          return !!entry && !entry.load_error;
        });
      } catch (cleanupError) {
        console.error('task-load-failure cleanup (second test) failed:', cleanupError);
      }
    }
  });
});
