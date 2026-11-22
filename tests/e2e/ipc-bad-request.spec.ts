/**
 * ipc-bad-request.spec.ts
 *
 * A task SDK call whose JSON has a wrongly typed field (here a numeric
 * kv.get key, which the shim passes through untyped) must fail just that
 * call. The run's IPC connection stays up, so later SDK calls and the
 * task's return value still go through and the run succeeds.
 */

import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { runReturnValue, runStatus, type Run } from './helpers/runs';

const SOURCE = 'e2e-tests';
const TASK_NAME = 'ipc-bad-request';
const TASK_ID = `${SOURCE}/${TASK_NAME}`;

const TASK_YAML = `apiVersion: dicode/v1
kind: Task
name: IPC Bad Request
description: Sends a badly typed SDK request, then keeps using the SDK.
runtime: deno
trigger:
  manual: true
timeout: 20s
`;

const TASK_TS = `export default async function main({ kv }) {
  let bad = "no error";
  try {
    await kv.get(123);
  } catch (e) {
    bad = String(e instanceof Error ? e.message : e);
  }
  await kv.set("probe", "still-connected");
  const value = await kv.get("probe");
  return { bad, value };
}
`;

function tasksetPath(): string {
  const p = process.env.DICODE_E2E_TASKSET_PATH;
  if (!p) throw new Error('DICODE_E2E_TASKSET_PATH not set — global setup may have failed');
  return p;
}

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
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`timed out waiting for ${what}`);
}

async function taskListed(request: APIRequestContext): Promise<boolean> {
  const res = await request.get('/api/tasks');
  if (!res.ok()) return false;
  const tasks = await res.json() as Array<{ id?: string }>;
  return tasks.some((t) => t.id === TASK_ID);
}

test.describe('IPC badly typed request', () => {
  test.setTimeout(90_000);
  let originalTaskset = '';

  test.beforeAll(() => {
    originalTaskset = fs.readFileSync(tasksetPath(), 'utf8');
    fs.mkdirSync(taskDir(), { recursive: true });
    fs.writeFileSync(path.join(taskDir(), 'task.yaml'), TASK_YAML, 'utf8');
    fs.writeFileSync(path.join(taskDir(), 'task.ts'), TASK_TS, 'utf8');
    const sep = originalTaskset.endsWith('\n') ? '' : '\n';
    fs.writeFileSync(
      tasksetPath(),
      `${originalTaskset}${sep}    ${TASK_NAME}:\n      ref:\n        path: ./${TASK_NAME}/task.yaml\n`,
      'utf8',
    );
  });

  test.afterAll(async ({ request }) => {
    fs.writeFileSync(tasksetPath(), originalTaskset, 'utf8');
    fs.rmSync(taskDir(), { recursive: true, force: true });
    await waitFor(async () => !(await taskListed(request)), 'task to deregister');
  });

  test('a badly typed SDK call errors alone and the run still succeeds', async ({ request }) => {
    await waitFor(() => taskListed(request), `${TASK_ID} to appear in /api/tasks`);

    const encoded = encodeURIComponent(TASK_ID);
    await waitFor(async () => {
      const res = await request.get(`/api/tasks/${encoded}`);
      if (!res.ok()) return false;
      if ((await res.json() as { pending_approval?: boolean }).pending_approval !== true) return true;
      await request.post(`/api/tasks/${encoded}/approve`);
      return false;
    }, `${TASK_ID} to be approved`);

    const runRes = await request.post(`/api/tasks/${encoded}/run`);
    expect(runRes.ok()).toBe(true);
    const { runId } = await runRes.json() as { runId: string };

    let run: Run = {};
    await waitFor(async () => {
      const res = await request.get(`/api/runs/${runId}`);
      if (!res.ok()) return false;
      run = await res.json() as Run;
      const s = runStatus(run) ?? '';
      return s !== '' && s !== 'running';
    }, `run ${runId} to finish`);

    expect(runStatus(run)).toBe('success');
    const result = JSON.parse(runReturnValue(run)) as { bad: string; value: string };
    expect(result.bad).toContain('ipc: invalid request');
    expect(result.value).toBe('still-connected');
  });
});
