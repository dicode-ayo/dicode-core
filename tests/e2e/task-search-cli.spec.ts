/**
 * task-search-cli.spec.ts
 *
 * End-to-end coverage for `dicode task search` driving the real binary
 * against the e2e fixture TaskSet. The command reads TaskSets directly and is
 * routed before the daemon dial in cmd/dicode/main.go, so it is run with a
 * data dir that has no daemon socket: it must neither need nor start one.
 */

import { test, expect } from '@playwright/test';
import { execFile } from 'node:child_process';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { promisify } from 'node:util';
import { BINARY } from './helpers/dicode-server';

const execFileAsync = promisify(execFile);
const CLI_TIMEOUT_MS = 20_000;
const JSON_KEYS = ['commit', 'description', 'id', 'location', 'name', 'ref', 'source'];

function requiredEnv(name: string): string {
  const v = process.env[name];
  expect(v, `${name} must be set by the e2e daemon helper`).toBeTruthy();
  return v!;
}

async function search(args: string[]): Promise<{ stdout: string; stderr: string }> {
  const noDaemonDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dicode-search-e2e-'));
  try {
    return await execFileAsync(BINARY, ['task', 'search', ...args], {
      env: { ...process.env, DICODE_DATA_DIR: noDaemonDir },
      timeout: CLI_TIMEOUT_MS,
    });
  } finally {
    fs.rmSync(noDaemonDir, { recursive: true, force: true });
  }
}

test.describe('dicode task search (CLI)', () => {
  test('finds a fixture task by name in an explicit --taskset', async () => {
    const tasksetPath = requiredEnv('DICODE_E2E_TASKSET_PATH');
    const { stdout } = await search(['hello manual', '--taskset', tasksetPath]);

    const lines = stdout.trimEnd().split('\n');
    expect(lines[0].split(/\s+/)).toEqual(['ID', 'NAME', 'DESCRIPTION']);
    expect(lines).toHaveLength(2);
    expect(lines[1]).toMatch(/\/hello-manual\s+Hello Manual\s+/);
  });

  test('--json over the configured sources has the stable keys', async () => {
    const configPath = requiredEnv('DICODE_E2E_CONFIG_PATH');
    const { stdout } = await search(['HELLO', 'manual', '--config', configPath, '--json']);

    // Other specs may add sources to the shared config, so only the fixture
    // row is pinned.
    const rows = JSON.parse(stdout) as Record<string, string>[];
    for (const row of rows) {
      expect(Object.keys(row).sort()).toEqual(JSON_KEYS);
    }
    const fixture = rows.filter((r) => r.id === 'e2e-tests/hello-manual');
    expect(fixture).toHaveLength(1);
    expect(fixture[0]).toMatchObject({
      source: 'e2e-tests',
      id: 'e2e-tests/hello-manual',
      name: 'Hello Manual',
      location: requiredEnv('DICODE_E2E_TASKSET_PATH'),
    });
  });

  test('a query nothing matches exits 0 with no rows', async () => {
    const tasksetPath = requiredEnv('DICODE_E2E_TASKSET_PATH');

    const text = await search(['no-such-task-anywhere', '--taskset', tasksetPath]);
    expect(text.stdout).toBe('');
    expect(text.stderr).toContain('no tasks match');

    const json = await search(['no-such-task-anywhere', '--taskset', tasksetPath, '--json']);
    expect(JSON.parse(json.stdout)).toEqual([]);
  });
});
