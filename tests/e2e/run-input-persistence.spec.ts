/**
 * run-input-persistence.spec.ts
 *
 * End-to-end tests for the run-input persistence pipeline (#233).
 *
 * Groups:
 *   1 — Persist + redact:
 *         POST a webhook with sensitive headers/body/query, assert the
 *         encrypted blob lands at ${DATADIR}/run-inputs/<runID>.bin, the
 *         plaintext sensitive values are NOT present in the file, and the
 *         runs row's InputRedactedFields lists the correct dotted paths.
 *   2 — Cleanup task is runnable:
 *         Trigger buildin/run-inputs-cleanup and verify it completes without
 *         error. The file persisted in Group 1 must still exist (retention
 *         default is 30 days; we just stored it).
 *   3 — Cleanup with retention=0 [SKIPPED]:
 *         Asserting that cleanup actually deletes the file requires setting
 *         defaults.run_inputs.retention: 0s in dicode.yaml, which the e2e
 *         helper does not currently parameterize. Deferred to a follow-up.
 */

import { test, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';

const WEBHOOK_PATH = '/hooks/persistence-test';

// ─── helpers ──────────────────────────────────────────────────────────────────

/**
 * Poll GET /api/runs/<runID> until the run reaches a terminal state.
 * Returns the run record once it is no longer "running".
 */
async function waitForRun(
  request: import('@playwright/test').APIRequestContext,
  runID: string,
  timeoutMs = 30_000,
): Promise<Record<string, unknown>> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await request.get(`/api/runs/${runID}`);
    if (res.ok()) {
      const run = (await res.json()) as Record<string, unknown>;
      const status = (run.Status ?? run.status) as string | undefined;
      if (status && status !== 'running') {
        return run;
      }
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`Run ${runID} did not finish within ${timeoutMs}ms`);
}

/**
 * POST to the persistence-test webhook with sensitive headers, body, and query.
 * Returns the X-Run-Id from the response header.
 */
async function postSensitiveWebhook(
  request: import('@playwright/test').APIRequestContext,
): Promise<string> {
  const res = await request.post(
    `${WEBHOOK_PATH}?api_key=sk_x&page=1`,
    {
      headers: {
        'Content-Type': 'application/json',
        'Authorization': 'Bearer xyz',
        'X-Slack-Signature': 't=1,v1=abc',
        'X-Custom-Token': 'c',
      },
      data: { user: 'alice', password: 'sec', token: 'abc' },
    },
  );
  expect(res.ok()).toBe(true);
  const runId = res.headers()['x-run-id'];
  expect(runId).toBeTruthy();
  return runId;
}

// ─── Group 1: Persist + redact ────────────────────────────────────────────────

test.describe('Group 1: persist + redact', () => {
  // Increase timeout — persistence involves multiple task executions under the
  // daemon (local-storage put), which can be slow in CI.
  test.setTimeout(60_000);

  test(
    'encrypted blob is written to ${DATADIR}/run-inputs/<runID>.bin',
    async ({ request }) => {
      const tempDir = process.env.DICODE_E2E_TEMP_DIR;
      if (!tempDir) {
        test.skip(true, 'DICODE_E2E_TEMP_DIR not set — cannot assert file existence');
        return;
      }

      const runId = await postSensitiveWebhook(request);

      // Wait for the run (and its async persistence task) to complete.
      await waitForRun(request, runId);

      // Poll for the blob file to appear — the storage task runs async.
      const blobPath = path.join(tempDir, 'run-inputs', `${runId}.bin`);
      await expect.poll(
        () => fs.existsSync(blobPath),
        { timeout: 15_000, intervals: [500] },
      ).toBe(true);
    },
  );

  test(
    'plaintext sensitive values are absent from the on-disk blob',
    async ({ request }) => {
      const tempDir = process.env.DICODE_E2E_TEMP_DIR;
      if (!tempDir) {
        test.skip(true, 'DICODE_E2E_TEMP_DIR not set');
        return;
      }

      const runId = await postSensitiveWebhook(request);
      await waitForRun(request, runId);

      const blobPath = path.join(tempDir, 'run-inputs', `${runId}.bin`);
      await expect.poll(
        () => fs.existsSync(blobPath),
        { timeout: 15_000, intervals: [500] },
      ).toBe(true);

      const raw = fs.readFileSync(blobPath);
      const rawStr = raw.toString('latin1');

      // None of the plaintext sensitive values must appear in the raw file.
      expect(rawStr).not.toContain('Bearer xyz');
      expect(rawStr).not.toContain('sec');   // body.password
      expect(rawStr).not.toContain('sk_x');  // query.api_key
      // "abc" also appears as the token/signature values.
      expect(rawStr).not.toContain('abc');
    },
  );

  test(
    'GET /api/runs/<runID> returns InputStorageKey, InputSize>0, InputStoredAt>0, InputPinned=0',
    async ({ request }) => {
      const runId = await postSensitiveWebhook(request);
      await waitForRun(request, runId);

      // Poll until InputStorageKey is set (the persistence callback runs async).
      await expect.poll(
        async () => {
          const res = await request.get(`/api/runs/${runId}`);
          if (!res.ok()) return null;
          const run = (await res.json()) as Record<string, unknown>;
          return run.InputStorageKey ?? null;
        },
        { timeout: 15_000, intervals: [500] },
      ).toBeTruthy();

      const res = await request.get(`/api/runs/${runId}`);
      expect(res.ok()).toBe(true);
      const run = (await res.json()) as Record<string, unknown>;

      // InputStorageKey must be non-empty and follow the "run-inputs/<runID>" pattern.
      expect(typeof run.InputStorageKey).toBe('string');
      expect(run.InputStorageKey as string).toContain('run-inputs/');

      // InputSize must be a positive integer (ciphertext size).
      expect(typeof run.InputSize).toBe('number');
      expect(run.InputSize as number).toBeGreaterThan(0);

      // InputStoredAt must be a positive Unix timestamp.
      expect(typeof run.InputStoredAt).toBe('number');
      expect(run.InputStoredAt as number).toBeGreaterThan(0);

      // InputPinned must be 0 (not pinned).
      expect(run.InputPinned).toBe(0);
    },
  );

  test(
    'InputRedactedFields lists all expected sensitive dotted paths',
    async ({ request }) => {
      const runId = await postSensitiveWebhook(request);
      await waitForRun(request, runId);

      // Poll until InputStorageKey is set (meaning persistence finished).
      await expect.poll(
        async () => {
          const res = await request.get(`/api/runs/${runId}`);
          if (!res.ok()) return null;
          const run = (await res.json()) as Record<string, unknown>;
          return run.InputStorageKey ?? null;
        },
        { timeout: 15_000, intervals: [500] },
      ).toBeTruthy();

      const res = await request.get(`/api/runs/${runId}`);
      expect(res.ok()).toBe(true);
      const run = (await res.json()) as Record<string, unknown>;

      // InputRedactedFields is a []string JSON array (Go PascalCase, no extra tags).
      const redacted = run.InputRedactedFields as string[] | null | undefined;
      expect(Array.isArray(redacted)).toBe(true);
      const fields = redacted as string[];

      // Header redactions expected by the deny-list.
      expect(fields).toContain('headers.Authorization');
      expect(fields).toContain('headers.X-Slack-Signature');
      // "X-Custom-Token" matches the "token" substring deny-list.
      expect(fields).toContain('headers.X-Custom-Token');

      // Query string: api_key matches the "key" substring deny-list.
      expect(fields).toContain('query.api_key');

      // Body: password and token match deny-list entries.
      expect(fields).toContain('body.password');
      expect(fields).toContain('body.token');
    },
  );
});

// ─── Group 2: Cleanup task is runnable ────────────────────────────────────────

// The cleanup task is registered under the e2e-tests namespace because it is
// loaded via the fixture taskset (not the real buildin/ source). Its full task
// ID is "e2e-tests/run-inputs-cleanup".
const CLEANUP_TASK_ID = 'e2e-tests/run-inputs-cleanup';

test.describe('Group 2: cleanup task is runnable', () => {
  test.setTimeout(60_000);

  test(
    'e2e-tests/run-inputs-cleanup runs to completion without error',
    async ({ request }) => {
      // Verify the cleanup task is registered.
      const taskRes = await request.get(
        `/api/tasks/${encodeURIComponent(CLEANUP_TASK_ID)}`,
      );
      if (!taskRes.ok()) {
        test.skip(true, `${CLEANUP_TASK_ID} not registered — fixture taskset missing it?`);
        return;
      }

      // Fire the cleanup task.
      const fireRes = await request.post(
        `/api/tasks/${encodeURIComponent(CLEANUP_TASK_ID)}/run`,
        { headers: { 'Content-Type': 'application/json' } },
      );
      expect(fireRes.ok()).toBe(true);
      const { runId } = (await fireRes.json()) as { runId: string };
      expect(runId).toBeTruthy();

      // Wait for the cleanup task to finish.
      const cleanupRun = await waitForRun(request, runId, 30_000);
      const status = (cleanupRun.Status ?? cleanupRun.status) as string;
      expect(status).toBe('success');
    },
  );

  test(
    'persisted blob is NOT deleted by cleanup (default 30-day retention is far in the future)',
    async ({ request }) => {
      const tempDir = process.env.DICODE_E2E_TEMP_DIR;
      if (!tempDir) {
        test.skip(true, 'DICODE_E2E_TEMP_DIR not set');
        return;
      }

      // POST a fresh webhook and wait for persistence.
      const runId = await postSensitiveWebhook(request);
      await waitForRun(request, runId);

      const blobPath = path.join(tempDir, 'run-inputs', `${runId}.bin`);
      await expect.poll(
        () => fs.existsSync(blobPath),
        { timeout: 15_000, intervals: [500] },
      ).toBe(true);

      // Skip if the cleanup task is not registered.
      const taskRes = await request.get(
        `/api/tasks/${encodeURIComponent(CLEANUP_TASK_ID)}`,
      );
      if (!taskRes.ok()) {
        test.skip(true, `${CLEANUP_TASK_ID} not registered — skipping negative-delete check`);
        return;
      }

      // Fire cleanup and wait for it.
      const fireRes = await request.post(
        `/api/tasks/${encodeURIComponent(CLEANUP_TASK_ID)}/run`,
        { headers: { 'Content-Type': 'application/json' } },
      );
      if (!fireRes.ok()) {
        test.skip(true, 'cleanup task failed to start — skipping');
        return;
      }
      const { runId: cleanupRunId } = (await fireRes.json()) as { runId: string };
      await waitForRun(request, cleanupRunId, 30_000);

      // The blob must still exist — default retention is 30 days.
      expect(fs.existsSync(blobPath)).toBe(true);
    },
  );
});

// ─── Group 3: Cleanup with retention=0 [SKIPPED] ─────────────────────────────

test.describe('Group 3: cleanup with retention=0', () => {
  test(
    'SKIP: asserting deletion requires dicode.yaml defaults.run_inputs.retention: 0s',
    async () => {
      // TODO(#233 follow-up): The e2e helper does not currently parameterize
      // the daemon config's defaults.run_inputs.retention. To exercise actual
      // blob deletion via cleanup, we would need to:
      //
      //   1. Write a dicode-unauth.yaml template with:
      //        defaults:
      //          run_inputs:
      //            retention: 0s
      //   2. Pass the template variant to dicode-server.ts writeConfig.
      //
      // Until then, deletion is covered by Go unit tests in
      // pkg/trigger/engine_input_persistence_test.go and
      // pkg/registry/registry_input_test.go.
      test.skip(
        true,
        'retention=0 cleanup requires dicode.yaml mutation; not yet parameterized in the e2e helper. ' +
          'Covered by Go unit tests; deferred to follow-up.',
      );
    },
  );
});
