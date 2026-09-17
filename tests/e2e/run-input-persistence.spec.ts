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
 *   3 — Cleanup with a near-zero retention:
 *         Asserting that cleanup actually deletes the file requires a very
 *         short defaults.run_inputs.retention, which the shared fixture
 *         (used by every other spec in this project) does not set. Runs
 *         against its own isolated daemon via helpers/dicode-server.ts's
 *         startIsolatedDaemon() (#850) instead of mutating the shared one.
 */

import { test, expect, request as pwRequest, type APIRequestContext } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import { startIsolatedDaemon, type IsolatedDaemon } from './helpers/dicode-server';

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

// ─── Group 1b: Value-shape redaction (#810) ───────────────────────────────────
//
// The deny-list above only redacts by field name. #810 adds a second,
// name-independent check: a value that embeds the "dcap_" approval-token
// prefix is redacted no matter what field carries it — the gap being that an
// approval link forwarded through an unlisted name (e.g. "link", "cta",
// "callback") previously reached the persisted blob in plaintext.

/**
 * POST to the persistence-test webhook with an approval-token-shaped value
 * under a query param name the deny-list has never heard of.
 */
async function postUnlistedCredentialWebhook(
  request: import('@playwright/test').APIRequestContext,
): Promise<string> {
  const res = await request.post(
    `${WEBHOOK_PATH}?cb=https%3A%2F%2Fhost%2Fapprove%2Fdcap_e2eRegressionToken`,
    {
      headers: { 'Content-Type': 'application/json' },
      data: { user: 'alice' },
    },
  );
  expect(res.ok()).toBe(true);
  const runId = res.headers()['x-run-id'];
  expect(runId).toBeTruthy();
  return runId;
}

test.describe('Group 1b: value-shape redaction (#810)', () => {
  test.setTimeout(60_000);

  test(
    'a credential-shaped value under an unlisted field name is absent from the on-disk blob',
    async ({ request }) => {
      const tempDir = process.env.DICODE_E2E_TEMP_DIR;
      if (!tempDir) {
        test.skip(true, 'DICODE_E2E_TEMP_DIR not set');
        return;
      }

      const runId = await postUnlistedCredentialWebhook(request);
      await waitForRun(request, runId);

      const blobPath = path.join(tempDir, 'run-inputs', `${runId}.bin`);
      await expect.poll(
        () => fs.existsSync(blobPath),
        { timeout: 15_000, intervals: [500] },
      ).toBe(true);

      const raw = fs.readFileSync(blobPath);
      expect(raw.toString('latin1')).not.toContain('dcap_e2eRegressionToken');
    },
  );

  test(
    'InputRedactedFields includes the unlisted field ("query.cb"), not just deny-listed names',
    async ({ request }) => {
      const runId = await postUnlistedCredentialWebhook(request);
      await waitForRun(request, runId);

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
      const fields = (run.InputRedactedFields ?? []) as string[];

      // "cb" is not on any deny-list (name or substring) — only the
      // value-shape check catches it.
      expect(fields).toContain('query.cb');
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

// ─── Group 3: Cleanup with a near-zero retention ──────────────────────────────
//
// Needs its own daemon rather than the shared one every other spec in this
// project runs against, so this test can set defaults.run_inputs.retention
// without reconfiguring the whole suite — see #850.
//
// Two pre-existing gaps surfaced while wiring this up, neither of which
// #850 is the right place to fix, so this test works around both rather
// than exercising them:
//
//  1. pkg/config/config.go's applyDefaults treats Retention's Go zero value
//     as "unset" ("if cfg.Defaults.RunInputs.Retention == 0 { ...= 30 days
//     }"), so an explicit `retention: 0s` is silently clobbered back to the
//     30-day default — a plain time.Duration can't distinguish "explicitly
//     zero" from "not set". Using "1s" instead avoids it.
//  2. pkg/daemon/daemon.go's applyBuiltinOverrides only rewrites the
//     buildin/run-inputs-cleanup task's retention_seconds default when
//     spec.ID == "buildin/run-inputs-cleanup" exactly — this fixture's copy
//     is namespaced e2e-tests/run-inputs-cleanup (see taskset.yaml), so
//     dicode.yaml's defaults.run_inputs.retention never reaches it no
//     matter what value is configured. The fire-time params override added
//     by #836 (apiRunTask) does reach it, so that's what actually drives
//     this test's retention window below.
//
// The overlay is still worth setting and asserting on here even though it
// can't reach this particular task: it's the direct regression coverage for
// startIsolatedDaemon()/writeConfig()'s overlay-merge behavior itself — the
// actual "missing primitive" #850 adds — via the effective config the
// isolated daemon reports back over /api/config.

test.describe('Group 3: cleanup with a near-zero retention', () => {
  test.setTimeout(60_000);

  let daemon: IsolatedDaemon;
  let api: APIRequestContext;

  test.beforeAll(async () => {
    // test.setTimeout above only covers each test body — a beforeAll/afterAll
    // hook has its own, separate default (30s) budget that it does NOT
    // extend. Spinning up a whole daemon (binary check, an uncached
    // dicode-buildin fetch the first time this worker needs it, process
    // spawn, waitForReady) can run past that on a loaded CI box, so this
    // hook raises its own timeout the same way.
    test.setTimeout(60_000);
    daemon = await startIsolatedDaemon({
      overlay: { defaults: { run_inputs: { retention: '1s' } } },
    });
    api = await pwRequest.newContext({ baseURL: daemon.baseURL });
  });

  test.afterAll(async () => {
    await api?.dispose();
    await daemon?.stop();
  });

  test('the config overlay reaches the isolated daemon\'s effective config', async () => {
    const res = await api.get('/api/config');
    expect(res.ok()).toBe(true);
    const cfg = (await res.json()) as { Defaults?: { RunInputs?: { Retention?: number } } };
    // config.Config has no json tags, so this is Go's default PascalCase;
    // time.Duration marshals as an int64 of nanoseconds.
    expect(cfg.Defaults?.RunInputs?.Retention).toBe(1_000_000_000);
  });

  test(
    'cleanup deletes the persisted blob once retention has already elapsed',
    async () => {
      const runId = await postSensitiveWebhook(api);
      await waitForRun(api, runId);

      const blobPath = path.join(daemon.tempDir, 'run-inputs', `${runId}.bin`);
      await expect.poll(
        () => fs.existsSync(blobPath),
        { timeout: 15_000, intervals: [500] },
      ).toBe(true);

      const taskRes = await api.get(`/api/tasks/${encodeURIComponent(CLEANUP_TASK_ID)}`);
      expect(taskRes.ok(), `${CLEANUP_TASK_ID} not registered on the isolated daemon`).toBe(true);

      // ListExpiredInputs (pkg/registry/registry.go) compares at
      // second-granularity ("input_stored_at < beforeUnix", both unix
      // seconds). Sleeping past the 1s retention_seconds override below
      // (with margin for clock-boundary rounding) makes the blob's stored
      // second strictly earlier than cleanup's cutoff, which is the
      // "already expired" condition this test exists to exercise.
      await new Promise((r) => setTimeout(r, 2_100));

      // retention_seconds as a fire-time param, not the daemon-config
      // overlay above — see gap 2 in the describe-block comment.
      const fireRes = await api.post(
        `/api/tasks/${encodeURIComponent(CLEANUP_TASK_ID)}/run`,
        {
          headers: { 'Content-Type': 'application/json' },
          data: { params: { retention_seconds: '1' } },
        },
      );
      expect(fireRes.ok()).toBe(true);
      const { runId: cleanupRunId } = (await fireRes.json()) as { runId: string };
      const cleanupRun = await waitForRun(api, cleanupRunId, 30_000);
      const status = (cleanupRun.Status ?? cleanupRun.status) as string;
      expect(status).toBe('success');

      // Before #850, this was the assertion nothing could make: cleanup ran
      // against a fire-time retention window that had already elapsed, so
      // it must have deleted the blob.
      expect(fs.existsSync(blobPath)).toBe(false);
    },
  );
});
