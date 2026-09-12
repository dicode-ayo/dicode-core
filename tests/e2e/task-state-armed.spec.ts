/**
 * task-state-armed.spec.ts
 *
 * E2E regression coverage for #714: before this fix, the only endpoint that
 * rendered a task's resolved review surface (permissions, triggers, env
 * declarations, file inventory) was GET /api/tasks/{id}/pending-state, which
 * 409s for anything that is not pending approval. An armed task — the
 * steady-state most tasks are in almost all the time — had no read path at
 * all, so an operator asking "what can this thing reach right now?" had
 * nowhere to look outside the brief pend/approve window.
 *
 * GET /api/tasks/{id}/state is the new endpoint: it never 409s on an armed
 * task, and (per the issue's third open question) it must never make the
 * armed render look like a real pending approval — pending_hash must be
 * empty, never a value that could be replayed into POST .../approve.
 *
 * Uses the shared hello-manual fixture read-only (no mutation, no cleanup
 * needed) — approval is disabled in the e2e fixture config
 * (tests/e2e/fixtures/dicode-unauth.yaml has no `approval.enabled: true`),
 * so every fixture task, hello-manual included, is armed from the moment it
 * registers. That makes this test safe to run in any order alongside specs
 * that pend other tasks.
 */

import { test, expect } from '@playwright/test';

const TASK_ID = 'e2e-tests/hello-manual';

type StateBody = {
  task_id?: string;
  pending_hash?: string;
  runtime?: string;
  triggers?: Array<{ kind: string; cron?: string }>;
};

test.describe('GET /api/tasks/{id}/state for an armed task (#714)', () => {
  test('renders the resolved state with no pending_hash', async ({ request }) => {
    const res = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}/state`);
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as StateBody;

    expect(body.task_id).toBe(TASK_ID);
    // The armed-task invariant this issue exists to guarantee: never a value
    // that looks like a usable pending hash.
    expect(body.pending_hash ?? '').toBe('');
    expect(body.runtime).toBeTruthy();
    expect(Array.isArray(body.triggers)).toBe(true);
  });

  test('matches /pending-state\'s 409 for the same armed task', async ({ request }) => {
    // Sanity check on the contrast this issue is about: the old endpoint
    // still refuses an armed task outright, while /state (above) answers it.
    const res = await request.get(`/api/tasks/${encodeURIComponent(TASK_ID)}/pending-state`);
    expect(res.status()).toBe(409);
  });
});
