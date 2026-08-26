/**
 * task-create-fresh-install.spec.ts
 *
 * Regression coverage for the #568 fix in pkg/config/config.go's
 * applyDefaults: `${DATADIR}/ai-tasks` used to be synthesized into the
 * "ai-scratch" source ONLY when the directory already existed on disk
 * (an `os.Stat` check). On a genuinely fresh install — no prior
 * `dicode task create` ever run, so the directory has never been created —
 * that condition is never true, synthesis silently never fires, and
 * CreateTask's default-source lookup (`source == "" -> "ai-scratch"`, then
 * `sourceMgr.Get("ai-scratch")`) 404s with "source not found" on a brand new
 * user's very first `dicode task create` call.
 *
 * This spins up its OWN daemon against a brand-new, empty data dir (deliberately
 * NOT the shared global e2e daemon from helpers/dicode-server.ts, whose data dir
 * already has ai-tasks/ from earlier setup) so the "no pre-existing ai-tasks
 * dir" precondition is real, then drives the actual `dicode task create <name>`
 * CLI (no --ai, so no AI/LLM call is involved — this exercises only the
 * mkdir + source-synthesis fix, not the AI-turn-threading code path, which
 * pkg/ipc's control_task_authoring_test.go covers with a fake engine).
 *
 * Before the fix this test fails with "source \"ai-scratch\" not found".
 */

import { test, expect } from '@playwright/test';
import { spawn, execFile, type ChildProcess } from 'child_process';
import { promisify } from 'node:util';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import * as net from 'net';
import { BINARY } from './helpers/dicode-server';

const execFileAsync = promisify(execFile);

const REPO_ROOT = path.resolve(__dirname, '../..');
const CLI_TIMEOUT_MS = 20_000;

let daemonProc: ChildProcess | null = null;
let tempDir = '';
let webuiPort = 0;

async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address() as net.AddressInfo;
      srv.close(() => resolve(addr.port));
    });
    srv.on('error', reject);
  });
}

async function waitForUrl(url: string, timeoutMs = 30_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.status < 500) return;
    } catch {
      // not up yet — retry
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`URL ${url} did not become ready within ${timeoutMs}ms`);
}

async function waitForProcessExit(proc: ChildProcess, timeoutMs = 8_000): Promise<void> {
  if (proc.exitCode !== null) return;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('process did not exit within timeout')), timeoutMs);
    proc.once('exit', () => {
      clearTimeout(timer);
      resolve();
    });
  });
}

/** Run the real dicode CLI against this spec's own fresh daemon. */
async function dicode(args: string[]): Promise<{ stdout: string; stderr: string }> {
  return execFileAsync(BINARY, args, {
    cwd: REPO_ROOT,
    env: { ...process.env, DICODE_DATA_DIR: tempDir },
    timeout: CLI_TIMEOUT_MS,
    windowsHide: true,
  });
}

test.describe('dicode task create on a fresh install (no pre-existing ai-tasks dir)', () => {
  test.setTimeout(60_000);

  test.beforeAll(async () => {
    // helpers/dicode-server.ts's globalSetup already built the binary for the
    // shared e2e daemon — reuse it rather than rebuilding.
    expect(fs.existsSync(BINARY), `dicode binary missing at ${BINARY}`).toBe(true);

    tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dicode-e2e-fresh-install-'));
    webuiPort = await freePort();

    // Deliberately minimal: no spec.entries at all, so nothing pre-creates
    // any directory under data_dir before the daemon's own applyDefaults
    // runs — the precondition this test exists to check.
    const cfgPath = path.join(tempDir, 'dicode.yaml');
    fs.writeFileSync(
      cfgPath,
      `data_dir: ${tempDir}
log_level: info
server:
  port: ${webuiPort}
  auth: false
`,
      'utf8',
    );

    expect(
      fs.existsSync(path.join(tempDir, 'ai-tasks')),
      'precondition: ai-tasks must not exist before the daemon ever starts',
    ).toBe(false);

    daemonProc = spawn(BINARY, ['daemon', '--config', cfgPath], {
      cwd: REPO_ROOT,
      env: { ...process.env, HOME: process.env.HOME ?? os.homedir(), GOMEMLIMIT: '256MiB' },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    daemonProc.stdout?.on('data', (d: Buffer) => process.stdout.write(`[fresh-install daemon] ${d}`));
    daemonProc.stderr?.on('data', (d: Buffer) => process.stderr.write(`[fresh-install daemon] ${d}`));

    await waitForUrl(`http://127.0.0.1:${webuiPort}/api/tasks`);
  });

  test.afterAll(async () => {
    if (daemonProc) {
      daemonProc.kill('SIGTERM');
      await waitForProcessExit(daemonProc).catch(() => daemonProc?.kill('SIGKILL'));
    }
    if (tempDir) {
      fs.rmSync(tempDir, { recursive: true, force: true });
    }
  });

  test('applyDefaults creates ai-tasks/ on daemon startup, before any task create call', async () => {
    // The fix runs unconditionally at config-load time (daemon startup), not
    // lazily on first use — so by the time the daemon answered /api/tasks
    // above, the directory must already exist.
    const aiTasksDir = path.join(tempDir, 'ai-tasks');
    expect(fs.existsSync(aiTasksDir), `${aiTasksDir} was not created by daemon startup`).toBe(true);
    expect(fs.statSync(aiTasksDir).isDirectory()).toBe(true);
  });

  test('dicode task create <name> succeeds with no pre-existing ai-tasks dir', async () => {
    // No --ai flag: this is the plain scaffold path, so no LLM/AI call is
    // involved — it only exercises CreateTask's "ai-scratch" default-source
    // resolution, which is what the #568 mkdir fix makes work.
    const { stdout, stderr } = await dicode(['task', 'create', 'fresh-install-e2e-task']);

    expect(stderr).not.toContain('source "ai-scratch" not found');
    const taskID = stdout.trim();
    expect(taskID).toBe('ai-scratch/fresh-install-e2e-task');

    // And the scaffolded files really landed on disk under the synthesized
    // source's directory.
    const taskDir = path.join(tempDir, 'ai-tasks', 'fresh-install-e2e-task');
    expect(fs.existsSync(path.join(taskDir, 'task.yaml'))).toBe(true);
    expect(fs.existsSync(path.join(taskDir, 'task.ts'))).toBe(true);
  });
});
