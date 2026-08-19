/**
 * task-create-registers.spec.ts
 *
 * POST /api/task/create scaffolds task.yaml + task.js into a source. Those
 * files alone do not make a task: a source resolves through its taskset's
 * spec.entries and nothing scans the directory tree, so an unlisted directory
 * stays invisible to the daemon and the returned task id refers to nothing.
 *
 * This walks the whole path against a live daemon — files on disk, the entry
 * in the source's taskset.yaml, the task in /api/tasks after a reconcile, and
 * a run that is actually accepted rather than answered with "task not found".
 */

import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';

const SOURCE = 'e2e-tests';
const TASK_NAME = 'created-by-e2e';
const TASK_ID = `${SOURCE}/${TASK_NAME}`;

/** Path to the taskset.yaml the e2e source resolves through. */
function tasksetPath(): string {
  const p = process.env.DICODE_E2E_TASKSET_PATH;
  if (!p) throw new Error('DICODE_E2E_TASKSET_PATH not set — global setup may have failed');
  return p;
}

/** Directory the scaffolded task lands in: a sibling of the taskset file. */
function taskDir(): string {
  return path.join(path.dirname(tasksetPath()), TASK_NAME);
}

async function waitFor(
  probe: () => Promise<boolean>,
  what: string,
  timeoutMs = 45_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await probe()) return;
    await new Promise((r) => setTimeout(r, 1_000));
  }
  throw new Error(`timed out waiting for ${what}`);
}

async function taskListed(request: APIRequestContext): Promise<boolean> {
  const res = await request.get('/api/tasks');
  if (!res.ok()) return false;
  const tasks = await res.json() as Array<{ id?: string }>;
  return tasks.some((t) => t.id === TASK_ID);
}

test.describe('Task create registration', () => {
  let originalTaskset = '';

  test.beforeAll(() => {
    originalTaskset = fs.readFileSync(tasksetPath(), 'utf8');
  });

  // Restore the fixture: later specs share this daemon and its task list.
  test.afterAll(async ({ request }) => {
    fs.writeFileSync(tasksetPath(), originalTaskset, 'utf8');
    fs.rmSync(taskDir(), { recursive: true, force: true });
    await waitFor(async () => !(await taskListed(request)), 'created task to deregister');
  });

  test('scaffolded task is registered and runnable', async ({ request }) => {
    const created = await request.post('/api/task/create', {
      data: { name: TASK_NAME, source: SOURCE },
    });
    expect(created.status()).toBe(201);
    expect((await created.json() as { task_id: string }).task_id).toBe(TASK_ID);

    expect(fs.existsSync(path.join(taskDir(), 'task.yaml'))).toBe(true);
    expect(fs.existsSync(path.join(taskDir(), 'task.js'))).toBe(true);
    expect(fs.readFileSync(tasksetPath(), 'utf8')).toContain(`./${TASK_NAME}/task.yaml`);

    await waitFor(() => taskListed(request), `${TASK_ID} to appear in /api/tasks`);

    // A brand-new task arms only once the trust-on-change gate is satisfied.
    const encoded = encodeURIComponent(TASK_ID);
    await waitFor(async () => {
      const res = await request.get(`/api/tasks/${encoded}`);
      if (!res.ok()) return false;
      if ((await res.json() as { pending_approval?: boolean }).pending_approval !== true) return true;
      await request.post(`/api/tasks/${encoded}/approve`);
      return false;
    }, `${TASK_ID} to be approved`);

    const run = await request.post(`/api/tasks/${encoded}/run`);
    expect(run.ok()).toBe(true);
    const { runId } = await run.json() as { runId: string };
    expect(runId).toBeTruthy();

    let status = '';
    await waitFor(async () => {
      const res = await request.get(`/api/runs/${runId}`);
      if (!res.ok()) return false;
      const body = await res.json() as { Status?: string; status?: string };
      status = body.status ?? body.Status ?? '';
      return status !== '' && status !== 'running';
    }, `run ${runId} to finish`);
    expect(status).toBe('success');
  });
});
