/**
 * task-yml-manifest.spec.ts
 *
 * E2E regression coverage for #765: a taskset ref that resolves to a
 * task.yml manifest (instead of the conventional task.yaml) could never
 * actually load. pkg/taskset/resolver.go's resolveYAMLPath already probed a
 * directory-valued ref for task.yml and accepted it, and DetectKind read its
 * `kind:` field fine — but the load step (task.LoadDirWithVars /
 * task.LoadPipelineDir) re-derived a hardcoded task.yaml path from the
 * resolved file's parent directory, discarding which manifest actually
 * matched. The entry silently failed to resolve, so it never registered.
 *
 * The fixture entry `yml-manifest-target` (see
 * tests/e2e/fixtures/tasks/taskset.yaml) is a directory-valued ref whose
 * directory (tests/e2e/fixtures/tasks/yml-manifest-target/) holds only
 * task.yml. Before the fix, this test would have failed: the task would be
 * absent from GET /api/tasks and present in GET /api/sources' failed_count
 * for the "e2e-tests" source with an "open task.yaml ...: no such file or
 * directory" error.
 */

import { test, expect } from '@playwright/test';

const TASK_ID = 'e2e-tests/yml-manifest-target';
const SOURCE_NAME = 'e2e-tests';

type TaskListEntry = { id: string; load_error?: string };
type SourceEntry = {
  name: string;
  failed_count?: number;
  failures?: Array<{ id: string; error: string }>;
};

test.describe('Taskset ref resolving to a bare task.yml manifest (#765)', () => {
  test('registers cleanly with no load error, and does not count against the source', async ({ request }) => {
    let tasks: TaskListEntry[] = [];
    let entry: TaskListEntry | undefined;

    await expect(async () => {
      const res = await request.get('/api/tasks');
      expect(res.ok()).toBe(true);
      tasks = (await res.json()) as TaskListEntry[];
      entry = tasks.find((t) => t.id === TASK_ID);
      expect(entry).toBeTruthy();
    }).toPass({ timeout: 15_000 });

    expect(entry!.load_error ?? '').toBe('');

    const sourcesRes = await request.get('/api/sources');
    expect(sourcesRes.ok()).toBe(true);
    const sources = (await sourcesRes.json()) as SourceEntry[];
    const source = sources.find((s) => s.name === SOURCE_NAME);
    expect((source?.failures ?? []).some((f) => f.id === TASK_ID)).toBe(false);
  });

  test('the task actually runs (its manifest — not just its kind header — was loaded)', async ({ request }) => {
    const res = await request.post(
      `/api/tasks/${encodeURIComponent(TASK_ID)}/run`,
      { headers: { 'Content-Type': 'application/json' } },
    );
    expect(res.ok()).toBe(true);
    const { runId } = (await res.json()) as { runId: string };
    expect(runId).toBeTruthy();

    await expect(async () => {
      const runRes = await request.get(`/api/runs/${runId}`);
      expect(runRes.ok()).toBe(true);
      const run = (await runRes.json()) as { Status?: string; status?: string };
      expect(run.Status ?? run.status).toBe('success');
    }).toPass({ timeout: 15_000 });
  });
});
