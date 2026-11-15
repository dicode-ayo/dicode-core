/**
 * remove-source.spec.ts
 *
 * DELETE /api/settings/sources/{name} must take the source's tasks with it:
 * gone from GET /api/tasks and from the trigger engine, so the task's
 * webhook stops resolving — without a daemon restart.
 */

import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { withGuaranteedCleanup } from './helpers/cleanup';

const TASK_NAME = 'remove-me';

function writeSourceFixture(dir: string, hookPath: string): string {
  const taskDir = path.join(dir, TASK_NAME);
  fs.mkdirSync(taskDir, { recursive: true });
  fs.writeFileSync(path.join(taskDir, 'task.yaml'), [
    'apiVersion: dicode/v1',
    'kind: Task',
    'name: Remove Me',
    'runtime: deno',
    'trigger:',
    `  webhook: ${hookPath}`,
    'timeout: 10s',
    '',
  ].join('\n'));
  fs.writeFileSync(path.join(taskDir, 'task.js'),
    "export default async function main() { return { ok: true }; }\n");
  const tasksetPath = path.join(dir, 'taskset.yaml');
  fs.writeFileSync(tasksetPath, [
    'apiVersion: dicode/v1',
    'kind: TaskSet',
    'metadata:',
    '  name: e2e-remove-source',
    'spec:',
    '  entries:',
    `    ${TASK_NAME}:`,
    '      ref:',
    `        path: ${path.join(taskDir, 'task.yaml')}`,
    '',
  ].join('\n'));
  return tasksetPath;
}

async function taskIds(request: APIRequestContext): Promise<string[]> {
  const res = await request.get('/api/tasks');
  expect(res.ok()).toBe(true);
  const tasks = await res.json() as Array<{ id?: string }>;
  return tasks.map((t) => t.id ?? '');
}

test.describe('Remove source', () => {
  test('deleting a source deregisters its tasks and their webhooks', async ({ request }) => {
    const stamp = Date.now();
    const name = `e2e-remove-${stamp}`;
    const taskId = `${name}/${TASK_NAME}`;
    const hookPath = `/hooks/e2e-remove-source-${stamp}`;
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dicode-e2e-remove-source-'));
    const tasksetPath = writeSourceFixture(dir, hookPath);
    let removed = false;

    await withGuaranteedCleanup(async () => {
      const add = await request.post('/api/settings/sources', { data: { name, path: tasksetPath } });
      expect(add.ok(), await add.text()).toBe(true);

      // Past the daemon's startup grace window a new task is held pending
      // approval with its triggers unarmed, so approve it first.
      const encodedId = encodeURIComponent(taskId);
      await expect(async () => {
        const res = await request.get(`/api/tasks/${encodedId}`);
        expect(res.ok()).toBe(true);
        const t = await res.json() as { pending_approval?: boolean };
        if (t.pending_approval) {
          await request.post(`/api/tasks/${encodedId}/approve`);
        }
        const live = await request.get(hookPath);
        expect(live.status(), 'webhook must resolve while the source exists').not.toBe(404);
      }).toPass({ timeout: 20_000 });

      const del = await request.delete(`/api/settings/sources/${encodeURIComponent(name)}`);
      expect(del.ok(), await del.text()).toBe(true);
      removed = true;

      // RemoveSource returns after deregistration, so no polling is needed.
      expect(await taskIds(request)).not.toContain(taskId);
      const gone = await request.get(hookPath);
      expect(gone.status()).toBe(404);
    }, async () => {
      if (!removed) {
        await request.delete(`/api/settings/sources/${encodeURIComponent(name)}`);
      }
      fs.rmSync(dir, { recursive: true, force: true });
    });
  });
});
