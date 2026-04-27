/**
 * dicode-server.ts
 *
 * Core setup/teardown logic for Playwright e2e tests.
 * Exports named `setup` and `teardown` functions used by
 * global-setup.ts and global-teardown.ts respectively.
 *
 * What setup does:
 *  1. Builds the dicode binary if missing or stale.
 *  2. Creates a temp directory per test run.
 *  3. Copies the test task fixtures into the temp dir (so tests can mutate them).
 *  4. Writes a concrete taskset.yaml and dicode.yaml from the fixture templates.
 *  5. Spawns the dicode process.
 *  6. Waits until /api/tasks returns < 500 (server is up).
 *  7. Writes state to a temp file so teardown can find the PID.
 *  8. Exports env vars so individual test files can locate the temp task dir.
 *
 * Environment variables consumed:
 *   DICODE_AUTH_MODE        — "authenticated" | "unauthenticated" (default)
 *   TEST_WEBHOOK_SECRET     — HMAC secret forwarded to the test server env
 *
 * Environment variables produced (readable in test files):
 *   DICODE_E2E_TEMP_DIR     — absolute path to the temp directory
 *   DICODE_E2E_TASKSET_PATH — absolute path to the resolved taskset.yaml
 *   DICODE_E2E_CONFIG_PATH  — absolute path to the resolved dicode.yaml
 *   DICODE_E2E_TASKS_DIR    — absolute path to the copied tasks/ subdir
 */

import { execFileSync, spawn } from 'child_process';
import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';

export const REPO_ROOT = path.resolve(__dirname, '../../..');
export const BINARY = path.join(REPO_ROOT, 'dicode');
const FIXTURES_DIR = path.join(REPO_ROOT, 'tests/e2e/fixtures');
const TASKS_DIR = path.join(FIXTURES_DIR, 'tasks');

// Fixed path for the Playwright storage state — see writeAuthState below.
export const AUTH_STATE_PATH = path.join(REPO_ROOT, 'tests/e2e/.auth-state.json');

const PORT = 8765;
const BASE_URL = `http://localhost:${PORT}`;

// File used to hand off state (PID, temp dir) from setup → teardown.
const STATE_FILE = path.join(os.tmpdir(), 'dicode-e2e-state.json');

interface E2EState {
  pid: number;
  tempDir: string;
  configPath: string;
  tasksetPath: string;
}

// ─── helpers ──────────────────────────────────────────────────────────────────

function buildBinary(): void {
  console.log('[e2e] Building dicode binary…');
  execFileSync('go', ['build', '-o', 'dicode', './cmd/dicode'], {
    cwd: REPO_ROOT,
    stdio: 'inherit',
    env: { ...process.env },
  });
  console.log('[e2e] Build complete.');
}

function ensureBinary(): void {
  if (!fs.existsSync(BINARY)) {
    buildBinary();
    return;
  }
  // Rebuild if any Go source is newer than the binary.
  try {
    const out = execFileSync(
      'find',
      [REPO_ROOT, '-name', '*.go', '-newer', BINARY, '-not', '-path', '*/vendor/*'],
      { cwd: REPO_ROOT, encoding: 'utf8' },
    )
      .split('\n')
      .find((l) => l.trim());
    if (out) {
      console.log(`[e2e] Source file newer than binary (${out}) — rebuilding.`);
      buildBinary();
    }
  } catch {
    buildBinary();
  }
}

function copyDirSync(src: string, dest: string): void {
  fs.mkdirSync(dest, { recursive: true });
  for (const entry of fs.readdirSync(src, { withFileTypes: true })) {
    const srcPath = path.join(src, entry.name);
    const destPath = path.join(dest, entry.name);
    if (entry.isDirectory()) {
      copyDirSync(srcPath, destPath);
    } else {
      fs.copyFileSync(srcPath, destPath);
    }
  }
}

/**
 * Copy task fixtures into tempDir/tasks/ and write a resolved taskset.yaml
 * (FIXTURES_TASKS_DIR and BUILDIN_WEBUI_TASK_YAML placeholders substituted).
 * Returns the path to the written taskset.yaml.
 */
function writeTaskset(tempDir: string): { tasksetPath: string; tasksDir: string } {
  const tasksDir = path.join(tempDir, 'tasks');
  copyDirSync(TASKS_DIR, tasksDir);

  const buildinWebuiTaskYaml = path.join(REPO_ROOT, 'tasks/buildin/webui/task.yaml');
  const buildinMcpTaskYaml = path.join(REPO_ROOT, 'tasks/buildin/mcp/task.yaml');
  const buildinAuthProvidersTaskYaml = path.join(REPO_ROOT, 'tasks/buildin/auth-providers/task.yaml');
  const template = fs.readFileSync(path.join(TASKS_DIR, 'taskset.yaml'), 'utf8');
  const content = template
    .replace(/FIXTURES_TASKS_DIR/g, tasksDir)
    .replace(/BUILDIN_WEBUI_TASK_YAML/g, buildinWebuiTaskYaml)
    .replace(/BUILDIN_MCP_TASK_YAML/g, buildinMcpTaskYaml)
    .replace(/BUILDIN_AUTH_PROVIDERS_TASK_YAML/g, buildinAuthProvidersTaskYaml);
  const tasksetPath = path.join(tempDir, 'taskset.yaml');
  fs.writeFileSync(tasksetPath, content, 'utf8');
  return { tasksetPath, tasksDir };
}

/**
 * Instantiate a config template (replacing TEMP_DATA_DIR and TEMP_TASKSET_PATH)
 * and write it to tempDir/dicode.yaml.
 */
function writeConfig(
  templateName: 'dicode-unauth.yaml' | 'dicode-auth.yaml',
  tempDir: string,
  tasksetPath: string,
): string {
  const template = fs.readFileSync(path.join(FIXTURES_DIR, templateName), 'utf8');
  const content = template
    .replace(/TEMP_DATA_DIR/g, tempDir)
    .replace(/TEMP_TASKSET_PATH/g, tasksetPath);
  const cfgPath = path.join(tempDir, 'dicode.yaml');
  fs.writeFileSync(cfgPath, content, 'utf8');
  return cfgPath;
}

async function waitForReady(url: string, timeoutMs = 30_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${url}/api/tasks`);
      if (res.status < 500) return; // server is up (401 is fine in auth mode)
    } catch {
      // connection refused — server not up yet
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error(`[e2e] dicode did not become ready within ${timeoutMs}ms`);
}

// ─── exported functions ────────────────────────────────────────────────────────

export async function setup(): Promise<void> {
  ensureBinary();

  const authMode = process.env.DICODE_AUTH_MODE ?? 'unauthenticated';
  const templateName: 'dicode-unauth.yaml' | 'dicode-auth.yaml' =
    authMode === 'authenticated' ? 'dicode-auth.yaml' : 'dicode-unauth.yaml';

  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dicode-e2e-'));
  const { tasksetPath, tasksDir } = writeTaskset(tempDir);
  const configPath = writeConfig(templateName, tempDir, tasksetPath);

  console.log(`[e2e] Starting dicode (${authMode})`);
  console.log(`[e2e] Temp dir: ${tempDir}`);
  console.log(`[e2e] Config:   ${configPath}`);

  const serverEnv: NodeJS.ProcessEnv = {
    ...process.env,
    HOME: process.env.HOME ?? os.homedir(),
    // Soft memory ceiling on the Go daemon — prevents runaway heap growth
    // when the webui task spawns many Deno subprocesses for browser assets.
    GOMEMLIMIT: process.env.GOMEMLIMIT ?? '512MiB',
    // Disable the unlock-endpoint rate limiter: auth.spec.ts fires many
    // login attempts in quick succession and would otherwise trip the
    // 5-per-minute cap mid-suite.
    DICODE_DISABLE_UNLOCK_LIMITER: '1',
  };
  if (process.env.TEST_WEBHOOK_SECRET) {
    serverEnv.TEST_WEBHOOK_SECRET = process.env.TEST_WEBHOOK_SECRET;
  }

  const child = spawn(BINARY, ['daemon', '--config', configPath], {
    cwd: REPO_ROOT,
    env: serverEnv,
    detached: false,
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  child.stdout?.on('data', (d: Buffer) => process.stdout.write(`[dicode] ${d}`));
  child.stderr?.on('data', (d: Buffer) => process.stderr.write(`[dicode] ${d}`));
  child.on('exit', (code) => {
    if (code !== null && code !== 0) {
      console.error(`[e2e] dicode exited unexpectedly with code ${code}`);
    }
  });

  if (!child.pid) {
    throw new Error('[e2e] Failed to start dicode process — no PID returned');
  }

  const state: E2EState = {
    pid: child.pid,
    tempDir,
    configPath,
    tasksetPath,
  };
  fs.writeFileSync(STATE_FILE, JSON.stringify(state), 'utf8');

  // Expose paths to test files via environment variables.
  process.env.DICODE_E2E_TEMP_DIR = tempDir;
  process.env.DICODE_E2E_TASKSET_PATH = tasksetPath;
  process.env.DICODE_E2E_CONFIG_PATH = configPath;
  process.env.DICODE_E2E_TASKS_DIR = tasksDir;

  await waitForReady(BASE_URL);
  console.log('[e2e] dicode is ready.');

  // Seed a logged-in storage state file. The webui task has trigger.auth: true
  // so even in the "unauthenticated" project (server.auth=false, no passphrase),
  // browser GETs to /hooks/webui must carry a session cookie. Empty-passphrase
  // POST to /api/auth/login is accepted when no passphrase is configured.
  //
  // Written to a FIXED path (under the project) so playwright.config.ts can
  // reference it at config-load time — globalSetup runs after config eval,
  // so an env-var-based path wouldn't work.
  const loginPassword = authMode === 'authenticated' ? 'test-passphrase-12345' : '';
  await writeAuthState(BASE_URL, loginPassword, AUTH_STATE_PATH);
  console.log(`[e2e] auth state: ${AUTH_STATE_PATH}`);
}

async function writeAuthState(baseURL: string, password: string, outPath: string): Promise<void> {
  const res = await fetch(`${baseURL}/api/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
  });
  if (!res.ok) {
    throw new Error(`[e2e] login failed: ${res.status} ${await res.text()}`);
  }
  // Parse Set-Cookie headers — Node's fetch returns them via getSetCookie().
  type FetchHeaders = Headers & { getSetCookie?: () => string[] };
  const setCookies = (res.headers as FetchHeaders).getSetCookie?.() ?? [];
  const url = new URL(baseURL);
  const cookies = setCookies.map((raw) => parseSetCookie(raw, url.hostname));
  const state = { cookies, origins: [] };
  fs.writeFileSync(outPath, JSON.stringify(state, null, 2), 'utf8');
}

function parseSetCookie(raw: string, defaultDomain: string) {
  const parts = raw.split(';').map((s) => s.trim());
  const [name, ...valueParts] = parts[0].split('=');
  const value = valueParts.join('=');
  const attrs: Record<string, string | boolean> = {};
  for (const p of parts.slice(1)) {
    const [k, ...rest] = p.split('=');
    attrs[k.toLowerCase()] = rest.length ? rest.join('=') : true;
  }
  return {
    name,
    value,
    domain: (attrs['domain'] as string) ?? defaultDomain,
    path: (attrs['path'] as string) ?? '/',
    expires: -1,
    httpOnly: !!attrs['httponly'],
    secure: !!attrs['secure'],
    sameSite: ((attrs['samesite'] as string) ?? 'Lax') as 'Strict' | 'Lax' | 'None',
  };
}

export async function teardown(): Promise<void> {
  if (!fs.existsSync(STATE_FILE)) {
    return;
  }
  let state: E2EState;
  try {
    state = JSON.parse(fs.readFileSync(STATE_FILE, 'utf8')) as E2EState;
  } catch {
    return;
  }

  console.log(`[e2e] Stopping dicode (PID ${state.pid})…`);
  try {
    process.kill(state.pid, 'SIGTERM');
  } catch {
    // Process may have already exited (ESRCH) — ignore.
  }
  // Give it a moment to flush buffered logs before we delete the data dir.
  await new Promise((r) => setTimeout(r, 600));

  if (state.tempDir && fs.existsSync(state.tempDir)) {
    fs.rmSync(state.tempDir, { recursive: true, force: true });
  }
  fs.rmSync(STATE_FILE, { force: true });
  fs.rmSync(AUTH_STATE_PATH, { force: true });
  console.log('[e2e] Cleanup complete.');
}
