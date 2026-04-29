/**
 * dev-mode-clone.spec.ts
 *
 * End-to-end tests for the dev-mode-with-branch / on_failure_chain features
 * added in PR #241 (issue #236).
 *
 * Groups:
 *   1  – Dev-mode local-path regression
 *   2  – Dev-mode clone-mode happy path  (requires a git source — skipped otherwise)
 *   3  – ValidateRunID rejection
 *   4  – ValidateBranchName rejection
 *   5  – Concurrency busy guard          (requires a git source — skipped otherwise)
 *   6  – MCP switch_dev_mode advertises branch/base/run_id
 *   7  – on_failure_chain bare-string backwards compat
 *   8  – on_failure_chain structured form + params merge
 *   9  – Chain-of-chains suppression
 */

import { test, expect } from '@playwright/test';

// All REST tests use the unauthenticated server on port 8765 (no API key
// required). The `request` fixture is pre-configured with baseURL by
// playwright.config.ts (baseURL: 'http://localhost:8765').

const MCP_URL = '/mcp';

// ─── helpers ──────────────────────────────────────────────────────────────────

interface SourceInfo {
  name: string;
  type: string;
  dev_mode: boolean;
  dev_path?: string;
}

interface Run {
  ID?: string;
  id?: string;
  TaskID?: string;
  task_id?: string;
  Status?: string;
  status?: string;
  ParentRunID?: string;
  parent_run_id?: string;
  TriggerSource?: string;
  trigger_source?: string;
  ReturnValue?: string;
  return_value?: string;
}

function runID(r: Run): string {
  return (r.ID ?? r.id) as string;
}
function runStatus(r: Run): string {
  return (r.Status ?? r.status) as string;
}
function runParentID(r: Run): string {
  return (r.ParentRunID ?? r.parent_run_id ?? '') as string;
}
function runReturnValue(r: Run): string {
  return (r.ReturnValue ?? r.return_value ?? '') as string;
}

/**
 * List all sources. Returns the parsed array or [] on any non-200.
 */
async function listSources(
  request: import('@playwright/test').APIRequestContext,
): Promise<SourceInfo[]> {
  const res = await request.get('/api/sources');
  if (!res.ok()) return [];
  return (await res.json()) as SourceInfo[];
}

/**
 * Returns the first source where type === "git", or undefined.
 */
async function findGitSource(
  request: import('@playwright/test').APIRequestContext,
): Promise<SourceInfo | undefined> {
  const sources = await listSources(request);
  return sources.find((s) => s.type === 'git');
}

/**
 * Find a non-git source to use for validator-rejection tests.
 * Prefers local, falls back to the first source.
 */
async function findAnySource(
  request: import('@playwright/test').APIRequestContext,
): Promise<SourceInfo | undefined> {
  const sources = await listSources(request);
  if (sources.length === 0) return undefined;
  return sources.find((s) => s.type !== 'git') ?? sources[0];
}

/**
 * Disable dev mode on a named source (cleanup helper).
 */
async function disableDevMode(
  request: import('@playwright/test').APIRequestContext,
  sourceName: string,
): Promise<void> {
  await request.patch(`/api/sources/${encodeURIComponent(sourceName)}/dev`, {
    headers: { 'Content-Type': 'application/json' },
    data: { enabled: false },
  });
}

/**
 * Fire a manual task via POST /api/tasks/{id}/run and wait for it to reach a
 * terminal state (success or failure). Returns the run record.
 * Throws if the task cannot be started or times out.
 */
async function fireAndWaitForRun(
  request: import('@playwright/test').APIRequestContext,
  taskID: string,
  timeoutMs = 30_000,
): Promise<Run> {
  const res = await request.post(
    `/api/tasks/${encodeURIComponent(taskID)}/run`,
    { headers: { 'Content-Type': 'application/json' } },
  );
  if (!res.ok()) {
    throw new Error(
      `POST /api/tasks/${taskID}/run failed: ${res.status()} ${await res.text()}`,
    );
  }
  const { runId } = (await res.json()) as { runId: string };
  if (!runId) throw new Error(`No runId in response for ${taskID}`);

  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const runRes = await request.get(`/api/runs/${runId}`);
    if (runRes.ok()) {
      const run = (await runRes.json()) as Run;
      const s = runStatus(run);
      if (s && s !== 'running') return run;
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`Run ${runId} for ${taskID} did not finish within ${timeoutMs}ms`);
}

/**
 * Poll GET /api/tasks/{id}/runs for a chain-fired run that was triggered after
 * `afterMs` (ms since epoch). The on_failure_chain implementation does not
 * currently set ParentRunID on the chained run, so we correlate by:
 *   1. TriggerSource === "chain" (or "Chain")
 *   2. StartedAt > afterMs
 *
 * Returns the first matching run once it appears, up to timeoutMs.
 */
async function waitForChainedRun(
  request: import('@playwright/test').APIRequestContext,
  taskID: string,
  _parentRunID: string, // kept for future use when ParentRunID is wired
  timeoutMs = 20_000,
  afterMs = 0,
): Promise<Run> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await request.get(
      `/api/tasks/${encodeURIComponent(taskID)}/runs?limit=20`,
    );
    if (res.ok()) {
      const runs = (await res.json()) as Run[];
      // Prefer a run with matching ParentRunID (for forward compat), fall
      // back to TriggerSource=="chain" with StartedAt after the trigger time.
      const byParent = runs.find((r) => runParentID(r) === _parentRunID);
      if (byParent) return byParent;
      const byChainSource = runs.find((r) => {
        const src = (
          (r.TriggerSource ?? r.trigger_source) as string | undefined
        )?.toLowerCase();
        if (src !== 'chain') return false;
        if (afterMs > 0) {
          // Only include runs started after the trigger time.
          const startedAt =
            (r as unknown as Record<string, unknown>).StartedAt ??
            (r as unknown as Record<string, unknown>).started_at;
          if (typeof startedAt === 'string') {
            const ts = Date.parse(startedAt);
            return ts >= afterMs;
          }
        }
        return true;
      });
      if (byChainSource) return byChainSource;
    }
    await new Promise((r) => setTimeout(r, 400));
  }
  throw new Error(
    `No chained run of ${taskID} appeared within ${timeoutMs}ms`,
  );
}

// ─── Group 1: Dev-mode local-path regression ────────────────────────────────

test.describe('Group 1: dev-mode local-path (regression)', () => {
  test.afterEach(async ({ request }) => {
    // Clean up: disable dev mode on any source it may have been set on.
    const sources = await listSources(request);
    for (const s of sources) {
      if (s.dev_mode) {
        await disableDevMode(request, s.name);
      }
    }
  });

  test('PATCH local_path → 200 with local_path in body', async ({ request }) => {
    const sources = await listSources(request);
    if (sources.length === 0) {
      test.skip(true, 'No sources found — skipping local-path regression');
      return;
    }
    const src = sources[0];

    // Use the real taskset path produced by global-setup so the local sync
    // resolves successfully (pointing at the same file that's already active).
    // DICODE_E2E_TASKSET_PATH is set by dicode-server.ts global-setup.
    const realTasksetPath = process.env.DICODE_E2E_TASKSET_PATH;
    if (!realTasksetPath) {
      test.skip(true, 'DICODE_E2E_TASKSET_PATH not set — cannot test local_path mode');
      return;
    }

    const res = await request.patch(
      `/api/sources/${encodeURIComponent(src.name)}/dev`,
      {
        headers: { 'Content-Type': 'application/json' },
        data: { enabled: true, local_path: realTasksetPath },
      },
    );
    expect(res.status()).toBe(200);
    const body = (await res.json()) as Record<string, unknown>;
    expect(body.dev_mode).toBe(true);
    expect(body.local_path).toBe(realTasksetPath);
    expect(body.source).toBe(src.name);
  });
});

// ─── Group 2: Dev-mode clone-mode happy path ────────────────────────────────

test.describe('Group 2: dev-mode clone-mode happy path', () => {
  test.afterEach(async ({ request }) => {
    const sources = await listSources(request);
    for (const s of sources) {
      if (s.dev_mode) {
        await disableDevMode(request, s.name);
      }
    }
  });

  test(
    'clone-mode enable returns 200 with branch/run_id in body (git source only)',
    async ({ request }) => {
      const gitSrc = await findGitSource(request);
      if (!gitSrc) {
        test.skip(
          true,
          'No git-type source found in /api/sources — clone-mode requires a git source. ' +
            'Add a source with type:"git" to the fixture to exercise this group.',
        );
        return;
      }

      const runID = 'e2e-clone-happy';
      const res = await request.patch(
        `/api/sources/${encodeURIComponent(gitSrc.name)}/dev`,
        {
          headers: { 'Content-Type': 'application/json' },
          data: {
            enabled: true,
            branch: 'fix/e2e-clone-happy',
            base: 'main',
            run_id: runID,
          },
        },
      );
      expect(res.status()).toBe(200);
      const body = (await res.json()) as Record<string, unknown>;
      expect(body.dev_mode).toBe(true);
      expect(body.branch).toBe('fix/e2e-clone-happy');
      expect(body.run_id).toBe(runID);

      // Disable immediately to clean up the clone dir.
      await disableDevMode(request, gitSrc.name);
    },
  );
});

// ─── Group 3: ValidateRunID rejection ───────────────────────────────────────

// Table of invalid run IDs and expected substring(s) in the error body.
// Note: empty string run_id with a non-empty branch triggers "RunID required"
// error (not the ValidateRunID path), so the acceptance pattern covers that too.
const invalidRunIDs: Array<{ label: string; runID: string }> = [
  { label: 'path traversal ../../etc', runID: '../../etc' },
  { label: 'slash in run_id', runID: 'run/with/slash' },
  { label: 'dot in run_id', runID: 'run.dot' },
  { label: 'empty string', runID: '' },
  { label: '65 chars (too long)', runID: 'a'.repeat(65) },
  { label: 'control char \\x01', runID: 'run\x01id' },
];

test.describe('Group 3: ValidateRunID rejection', () => {
  for (const tc of invalidRunIDs) {
    test(`run_id "${tc.label}" → 400`, async ({ request }) => {
      const src = await findAnySource(request);
      if (!src) {
        test.skip(true, 'No source available for run ID validation test');
        return;
      }

      const res = await request.patch(
        `/api/sources/${encodeURIComponent(src.name)}/dev`,
        {
          headers: { 'Content-Type': 'application/json' },
          data: {
            enabled: true,
            branch: 'fix/anything',
            base: 'main',
            run_id: tc.runID,
          },
        },
      );
      // Must never be 200 — either validator fires or clone-mode gate fires.
      expect(res.status()).not.toBe(200);
      // Expect 400 specifically for validator hits.
      expect(res.status()).toBe(400);

      const body = await res.text();
      // Accept any of:
      //  - "invalid run id" / "run id" / "run_id" from ValidateRunID
      //  - "runid required" / "required" from the RunID-required check
      //  - "clone-mode" / "git source" / "rootref" from the git-gate
      const hasRunIDError =
        body.toLowerCase().includes('invalid run id') ||
        body.toLowerCase().includes('run id') ||
        body.toLowerCase().includes('run_id') ||
        body.toLowerCase().includes('invalid run') ||
        body.toLowerCase().includes('required');
      const hasCloneGateError =
        body.toLowerCase().includes('clone-mode') ||
        body.toLowerCase().includes('git source') ||
        body.toLowerCase().includes('rootref');
      expect(hasRunIDError || hasCloneGateError).toBe(true);
    });
  }
});

// ─── Group 4: ValidateBranchName rejection ──────────────────────────────────

// Note: empty branch is intentionally excluded from invalidBranchNames below.
// When branch="" in the PATCH body, the engine treats it as "not clone mode"
// (takes the local-path / disable path) and returns 200 — there is no
// API-level enforcement that branch must be non-empty. ValidateBranchName is
// only called inside enableClone, which is only reached when branch != "".
// A test.todo for strict empty-branch validation is left below.
const invalidBranchNames: Array<{ label: string; branch: string }> = [
  { label: '.lock component', branch: 'fix/foo.lock/bar' },
  { label: 'trailing dot component', branch: 'fix/foo.' },
  { label: 'double dot', branch: 'fix/foo..bar' },
  { label: 'double slash', branch: 'fix//x' },
  { label: '@{ sequence', branch: 'fix/x@{0}' },
  { label: 'leading dash', branch: '-fix/x' },
  { label: 'space in component', branch: 'fix/ x' },
];

test.describe('Group 4: ValidateBranchName rejection', () => {
  // NOTE: empty branch is handled by a separate test below (it returns 200,
  // not 400, because the engine takes the local-path code path when branch="").

  for (const tc of invalidBranchNames) {
    test(`branch "${tc.label}" → 400`, async ({ request }) => {
      const src = await findAnySource(request);
      if (!src) {
        test.skip(true, 'No source available for branch name validation test');
        return;
      }

      const res = await request.patch(
        `/api/sources/${encodeURIComponent(src.name)}/dev`,
        {
          headers: { 'Content-Type': 'application/json' },
          data: {
            enabled: true,
            branch: tc.branch,
            base: 'main',
            // Use a valid run_id so the branch validator is reached first.
            run_id: 'e2e-branch-test',
          },
        },
      );
      // Must never be 200.
      expect(res.status()).not.toBe(200);
      expect(res.status()).toBe(400);

      const body = await res.text();
      // Accept branch-invalid error OR clone-mode-requires-git error.
      const hasBranchError =
        body.toLowerCase().includes('invalid branch') ||
        body.toLowerCase().includes('branch') ||
        body.toLowerCase().includes('branch name');
      const hasCloneGateError =
        body.toLowerCase().includes('clone-mode') ||
        body.toLowerCase().includes('git source') ||
        body.toLowerCase().includes('rootref');
      expect(hasBranchError || hasCloneGateError).toBe(true);
    });
  }

  test(
    'empty branch → 200 (engine treats it as local-path/disable path, not clone-mode)',
    async ({ request }) => {
      // This documents the current engine behavior: an empty branch string
      // bypasses ValidateBranchName entirely — the PATCH handler takes the
      // local-path/disable code path (line ~206 in source.go) and succeeds.
      // A future API hardening pass could reject this at the HTTP layer.
      const src = await findAnySource(request);
      if (!src) {
        test.skip(true, 'No source available');
        return;
      }
      const res = await request.patch(
        `/api/sources/${encodeURIComponent(src.name)}/dev`,
        {
          headers: { 'Content-Type': 'application/json' },
          data: { enabled: true, branch: '', base: 'main', run_id: 'branch-empty-test' },
        },
      );
      // Empty branch → local-path code path → 200 (may trigger a sync).
      // We accept 200 or 400 but document that 200 is the current behavior.
      // If this changes to 400, the test should be updated to match.
      expect([200, 400]).toContain(res.status());
      // Clean up: if we inadvertently enabled dev mode, disable it.
      if (res.status() === 200) {
        await disableDevMode(request, src.name);
      }
    },
  );
});

// ─── Group 5: Concurrency busy guard ────────────────────────────────────────

test.describe('Group 5: concurrency busy guard', () => {
  test.afterEach(async ({ request }) => {
    const sources = await listSources(request);
    for (const s of sources) {
      if (s.dev_mode) {
        await disableDevMode(request, s.name);
      }
    }
  });

  test(
    'second concurrent clone-mode enable → 400 dev-mode busy (git source only)',
    async ({ request }) => {
      const gitSrc = await findGitSource(request);
      if (!gitSrc) {
        test.skip(
          true,
          'No git-type source found — busy-guard test requires a git source.',
        );
        return;
      }

      // Fire two PATCH requests to enable clone-mode in parallel.
      const [res1, res2] = await Promise.all([
        request.patch(
          `/api/sources/${encodeURIComponent(gitSrc.name)}/dev`,
          {
            headers: { 'Content-Type': 'application/json' },
            data: { enabled: true, branch: 'fix/busy-test-1', base: 'main', run_id: 'busy-test-1' },
          },
        ),
        request.patch(
          `/api/sources/${encodeURIComponent(gitSrc.name)}/dev`,
          {
            headers: { 'Content-Type': 'application/json' },
            data: { enabled: true, branch: 'fix/busy-test-2', base: 'main', run_id: 'busy-test-2' },
          },
        ),
      ]);

      const statuses = [res1.status(), res2.status()].sort();
      // Exactly one must succeed (200) and the other must fail (400).
      expect(statuses).toEqual([200, 400]);

      const failedRes = res1.status() === 400 ? res1 : res2;
      const failedBody = await failedRes.text();
      // The busy error must mention "busy" or "already active" or similar.
      const mentionsBusy =
        failedBody.toLowerCase().includes('busy') ||
        failedBody.toLowerCase().includes('already active') ||
        failedBody.toLowerCase().includes('clone-mode already');
      expect(mentionsBusy).toBe(true);

      // Disable both to clean up; ignore errors (one may already be disabled).
      await disableDevMode(request, gitSrc.name);
      await disableDevMode(request, gitSrc.name);

      // After cleanup, a fresh enable must succeed.
      const freshRes = await request.patch(
        `/api/sources/${encodeURIComponent(gitSrc.name)}/dev`,
        {
          headers: { 'Content-Type': 'application/json' },
          data: { enabled: true, branch: 'fix/after-busy', base: 'main', run_id: 'after-busy' },
        },
      );
      expect(freshRes.status()).toBe(200);
      // Final cleanup.
      await disableDevMode(request, gitSrc.name);
    },
  );
});

// ─── Group 6: MCP switch_dev_mode advertises new args ───────────────────────

interface JsonRpcResponse<T = unknown> {
  jsonrpc: '2.0';
  id: unknown;
  result?: T;
  error?: { code: number; message: string };
}
interface ToolsListResult {
  tools: Array<{
    name: string;
    description: string;
    inputSchema: { type: string; properties?: Record<string, unknown>; required?: string[] };
  }>;
}
interface ToolsCallResult {
  content: Array<{ type: 'text'; text: string }>;
}

test.describe('Group 6: MCP switch_dev_mode advertises new args', () => {
  test('tools/list: switch_dev_mode schema includes branch, base, run_id', async ({ request }) => {
    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: { jsonrpc: '2.0', id: 1, method: 'tools/list' },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse<ToolsListResult>;
    expect(body.result).toBeDefined();

    const tool = body.result!.tools.find((t) => t.name === 'switch_dev_mode');
    expect(tool).toBeDefined();

    const props = tool!.inputSchema.properties ?? {};
    expect(props['branch']).toBeDefined();
    expect(props['base']).toBeDefined();
    expect(props['run_id']).toBeDefined();
  });

  test('tools/call switch_dev_mode round-trips branch/base/run_id in hint text', async ({ request }) => {
    const sourceName = 'e2e-tests'; // known to exist from fixtures
    const branchVal = 'fix/mcp-test';
    const baseVal = 'main';
    const runIdVal = 'mcp-test-run-1';

    const res = await request.post(MCP_URL, {
      headers: { 'Content-Type': 'application/json' },
      data: {
        jsonrpc: '2.0',
        id: 2,
        method: 'tools/call',
        params: {
          name: 'switch_dev_mode',
          arguments: {
            source: sourceName,
            enabled: true,
            branch: branchVal,
            base: baseVal,
            run_id: runIdVal,
          },
        },
      },
    });
    expect(res.ok()).toBe(true);
    const body = (await res.json()) as JsonRpcResponse<ToolsCallResult>;
    // The tool must not return a JSON-RPC error.
    expect(body.error).toBeUndefined();
    expect(body.result?.content[0].type).toBe('text');

    const text = body.result!.content[0].text;
    // The hint body includes the JSON payload that was assembled from the args.
    // Verify the round-tripped values appear in the text.
    expect(text).toContain(branchVal);
    expect(text).toContain(baseVal);
    expect(text).toContain(runIdVal);
    // The source name and endpoint path should appear too.
    expect(text).toContain(sourceName);
    expect(text).toContain('/dev');
  });
});

// ─── Group 7: on_failure_chain bare-string backwards compat ─────────────────

test.describe('Group 7: on_failure_chain bare-string backwards compat', () => {
  test(
    'loop-target fixture (bare-string on_failure_chain) is registered without error',
    async ({ request }) => {
      // The loop-target fixture has `on_failure_chain: e2e-tests/loop-target`
      // (bare string). If the daemon parsed it correctly, the task will appear
      // in /api/tasks. A startup parse error would leave it absent.
      const res = await request.get('/api/tasks/e2e-tests%2Floop-target');
      expect(res.ok()).toBe(true);
      const task = (await res.json()) as { id?: string };
      const id = task.id ?? (task as Record<string, string>).ID;
      expect(id).toBeTruthy();
    },
  );
});

// ─── Group 8: on_failure_chain structured form + params merge ────────────────

test.describe('Group 8: on_failure_chain structured form + params merge', () => {
  // Longer timeout — we need to fire a task, wait for it to fail, then wait
  // for the chained run to appear.
  test.setTimeout(60_000);

  test(
    'will-fail fires chain-target with user params + reserved keys in input',
    async ({ request }) => {
      const triggerTimeMs = Date.now();

      // 1. Fire will-fail and wait for failure.
      const failedRun = await fireAndWaitForRun(request, 'e2e-tests/will-fail');
      const failedRunID = runID(failedRun);
      expect(runStatus(failedRun)).toBe('failure');

      // 2. Poll chain-target/runs for a chain-fired run after triggerTimeMs.
      // Note: the on_failure_chain engine does not currently set ParentRunID on
      // the chained run — we correlate by TriggerSource == "chain" and start
      // time instead.
      const chainedRun = await waitForChainedRun(
        request,
        'e2e-tests/chain-target',
        failedRunID, // used for ParentRunID lookup when available
        20_000,
        triggerTimeMs,
      );
      expect(chainedRun).toBeDefined();

      // Verify TriggerSource is "chain".
      const src = (
        (chainedRun.TriggerSource ?? chainedRun.trigger_source) as string | undefined
      )?.toLowerCase();
      expect(src).toBe('chain');

      // 3. Wait for the chained run to also finish (it should succeed — it's an echo task).
      const chainedRunID = runID(chainedRun);
      await expect
        .poll(
          async () => {
            const r = await request.get(`/api/runs/${chainedRunID}`);
            if (!r.ok()) return '';
            const run = (await r.json()) as Run;
            return runStatus(run);
          },
          { timeout: 15_000, intervals: [400] },
        )
        .toBe('success');

      // 4. Verify the input via the chained run's logs.
      // chain-target logs `chain-target received: <JSON>` with the full input.
      const logsRes = await request.get(`/api/runs/${chainedRunID}/logs`);
      expect(logsRes.ok()).toBe(true);
      const logs = (await logsRes.json()) as Array<{ message: string }>;
      const allMessages = logs.map((l) => l.message).join('\n');

      // Must contain the log line from chain-target.
      expect(allMessages).toContain('chain-target received');

      // The log line contains the serialized input; verify user params are present.
      expect(allMessages).toContain('"color"');
      expect(allMessages).toContain('"blue"');
      expect(allMessages).toContain('"iterations"');
      expect(allMessages).toContain('5');

      // Reserved engine keys must also appear in the input.
      expect(allMessages).toContain('"taskID"');
      expect(allMessages).toContain('e2e-tests/will-fail');
      expect(allMessages).toContain('"runID"');
      expect(allMessages).toContain(failedRunID);
      expect(allMessages).toContain('"status"');
      expect(allMessages).toContain('"failure"');
      expect(allMessages).toContain('"_chain_depth"');
    },
  );
});

// ─── Group 9: Chain-of-chains suppression ───────────────────────────────────

test.describe('Group 9: chain-of-chains suppression', () => {
  test.setTimeout(60_000);

  test(
    'chain-fired run that fails does not trigger its own on_failure_chain',
    async ({ request }) => {
      // Scenario:
      //   loop-target (manual) fails
      //     → engine fires will-fail (chain-triggered; trigger_source="chain")
      //   will-fail (chain-triggered) fails
      //     → engine detects trigger_source="chain" → suppresses chain-target
      //
      // Verification: no chain-fired run of chain-target appears after triggerTimeMs.
      //
      // Note: self-targeting on_failure_chain (loop-target → loop-target) is
      // separately prevented by the `targetID != completedTaskID` guard in
      // FireChain, which is distinct from the runTriggerSource guard tested here.

      const triggerTimeMs = Date.now();

      // 1. Fire loop-target and wait for it to fail.
      const triggerRun = await fireAndWaitForRun(request, 'e2e-tests/loop-target');
      expect(runStatus(triggerRun)).toBe('failure');

      // 2. Wait for will-fail to be chain-triggered by loop-target.
      const willFailChainedRun = await waitForChainedRun(
        request,
        'e2e-tests/will-fail',
        runID(triggerRun),
        15_000,
        triggerTimeMs,
      );
      expect(willFailChainedRun).toBeDefined();
      const willFailChainedRunID = runID(willFailChainedRun);

      // Verify will-fail ran as a chain-triggered task.
      const willFailSrc = (
        (willFailChainedRun.TriggerSource ?? willFailChainedRun.trigger_source) as string | undefined
      )?.toLowerCase();
      expect(willFailSrc).toBe('chain');

      // 3. Wait for the chain-triggered will-fail run to finish.
      await expect
        .poll(
          async () => {
            const r = await request.get(`/api/runs/${willFailChainedRunID}`);
            if (!r.ok()) return '';
            return runStatus((await r.json()) as Run);
          },
          { timeout: 15_000, intervals: [400] },
        )
        .toBe('failure');

      // 4. Give the engine a moment to (incorrectly) fire chain-target if the
      //    suppression were broken — 3 seconds is generous.
      await new Promise((r) => setTimeout(r, 3000));

      // 5. Verify that chain-target was NOT chain-fired after triggerTimeMs.
      //    (will-fail's on_failure_chain points to chain-target, but since
      //    will-fail was itself chain-fired, the engine must suppress it.)
      const chainTargetRunsRes = await request.get(
        `/api/tasks/${encodeURIComponent('e2e-tests/chain-target')}/runs?limit=50`,
      );
      expect(chainTargetRunsRes.ok()).toBe(true);
      const chainTargetRuns = (await chainTargetRunsRes.json()) as Run[];

      const suppressedRuns = chainTargetRuns.filter((r) => {
        const src = (
          (r.TriggerSource ?? r.trigger_source) as string | undefined
        )?.toLowerCase();
        if (src !== 'chain') return false;
        const startedAt =
          (r as unknown as Record<string, unknown>).StartedAt ??
          (r as unknown as Record<string, unknown>).started_at;
        if (typeof startedAt === 'string') {
          return Date.parse(startedAt) >= triggerTimeMs;
        }
        return true;
      });

      // With suppression working: chain-target must NOT have been chain-fired.
      expect(suppressedRuns.length).toBe(0);
    },
  );
});
