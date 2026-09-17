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
 *  5. Copies the add-source-tasks fixture into its OWN, separate temp
 *     directory and writes its own resolved taskset.yaml (issue #621: this
 *     must never be the same path/watch-root as the taskset.yaml from step 3
 *     — see the doc comment on writeAddSourceTaskset below).
 *  6. Spawns the dicode process.
 *  7. Waits until /api/tasks returns < 500 (server is up).
 *  8. Writes state to a temp file so teardown can find the PID(s)/dir(s).
 *  9. Exports env vars so individual test files can locate the temp task dirs.
 *
 * Environment variables consumed:
 *   DICODE_AUTH_MODE        — "authenticated" | "unauthenticated" (default)
 *   TEST_WEBHOOK_SECRET     — HMAC secret forwarded to the test server env
 *
 * Environment variables produced (readable in test files):
 *   DICODE_E2E_TEMP_DIR                — absolute path to the temp directory
 *   DICODE_E2E_TASKSET_PATH            — absolute path to the resolved taskset.yaml
 *   DICODE_E2E_CONFIG_PATH             — absolute path to the resolved dicode.yaml
 *   DICODE_E2E_TASKS_DIR               — absolute path to the copied tasks/ subdir
 *   DICODE_E2E_ADD_SOURCE_TASKSET_PATH — absolute path to the add-source-tasks
 *                                        fixture's own resolved taskset.yaml,
 *                                        in its own separate temp dir (issue
 *                                        #621 — see writeAddSourceTaskset)
 *
 * startIsolatedDaemon() (#850) — for a spec that needs one config key the
 * shared fixture doesn't set: rather than editing tests/e2e/fixtures/
 * dicode-unauth.yaml/dicode-auth.yaml (which reconfigures every other spec
 * on the same Playwright project, since they all share the one daemon
 * setup()/teardown() start and stop), a spec can spin up its OWN fully-
 * fixtured daemon — same task/taskset fixtures, its own free port and temp
 * data dir — with an `overlay` object deep-merged over the base config
 * template before it's written. See run-input-persistence.spec.ts's Group 3
 * for the motivating case.
 */

import { execFileSync, spawn, type ChildProcess } from 'child_process';
import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';
import * as net from 'net';
import { load as loadYAML, dump as dumpYAML } from 'js-yaml';
import { ensureBuildinCheckout } from './buildin';

export const REPO_ROOT = path.resolve(__dirname, '../../..');
export const BINARY = path.join(REPO_ROOT, 'dicode');
const FIXTURES_DIR = path.join(REPO_ROOT, 'tests/e2e/fixtures');
const TASKS_DIR = path.join(FIXTURES_DIR, 'tasks');
const ADD_SOURCE_FIXTURES_DIR = path.join(FIXTURES_DIR, 'add-source-tasks');

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
  // Separate root from tempDir (see writeAddSourceTaskset's doc comment) —
  // tracked here so teardown can remove it independently of tempDir.
  addSourceTempDir: string;
}

// A daemon spun up by startIsolatedDaemon() — see the module doc comment.
export interface IsolatedDaemon {
  baseURL: string;
  tempDir: string;
  tasksDir: string;
  tasksetPath: string;
  configPath: string;
  /** Kills the daemon process and removes its temp dir. Idempotent. */
  stop(): Promise<void>;
}

// ─── helpers ──────────────────────────────────────────────────────────────────

/**
 * Finds a free TCP port on localhost by binding to port 0 and reading it
 * back. There's an inherent TOCTOU gap between this and the daemon actually
 * binding the port a moment later — a collision surfaces as waitForReady's
 * generic timeout rather than a clear EADDRINUSE, but on this suite's
 * single-worker, sequential run the only realistic contender is a leftover
 * process from an earlier failed run, not a concurrent one.
 *
 * Exported: relay-buildin.spec.ts, relay-protocol.spec.ts, and
 * task-create-fresh-install.spec.ts each spin up their own standalone
 * daemon on an ephemeral port the same way startIsolatedDaemon() does, and
 * import this instead of keeping their own copy.
 */
export async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address() as net.AddressInfo;
      srv.close(() => resolve(addr.port));
    });
    srv.on('error', reject);
  });
}

/**
 * Recursively merges `overlay` onto `base` (`overlay` wins on conflicts).
 * Plain objects are merged key-by-key; arrays and scalars in `overlay`
 * replace the corresponding value in `base` outright rather than combining
 * with it — there is no sensible generic way to "merge" two YAML sequences.
 */
function deepMerge(base: unknown, overlay: unknown): unknown {
  if (isPlainObject(overlay) && isPlainObject(base)) {
    const merged: Record<string, unknown> = { ...base };
    for (const [key, value] of Object.entries(overlay)) {
      merged[key] = deepMerge(base[key], value);
    }
    return merged;
  }
  return overlay;
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

async function waitForProcessExit(proc: ChildProcess, timeoutMs = 8_000): Promise<void> {
  if (proc.exitCode !== null || proc.signalCode !== null) return;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('process did not exit within timeout')), timeoutMs);
    proc.once('exit', () => {
      clearTimeout(timer);
      resolve();
    });
  });
}

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
/**
 * initFixtureRepo makes dir a git repository with every fixture in its HEAD
 * tree and an `origin` pointing at a github.com URL.
 *
 * internal/gitops.HeadCommit resolves a task's commit only when the task
 * directory appears in HEAD's tree, so without this the commit-range
 * decoration on /approve/{token} is unreachable end-to-end and only its
 * degraded, absent form can be asserted. A dirty tree is fine — later
 * mutations by a spec keep resolving the commit they were last committed at,
 * which is exactly what the decoration reports.
 *
 * The remote is never fetched from; it exists so the compare link has a host
 * whose URL shape pkg/approval recognizes.
 */
function initFixtureRepo(dir: string): void {
  const git = (...args: string[]): void => {
    execFileSync('git', args, { cwd: dir, stdio: 'ignore' });
  };
  git('init', '-q', '-b', 'main');
  git('config', 'user.email', 'e2e@dicode.test');
  git('config', 'user.name', 'dicode e2e');
  git('config', 'commit.gpgsign', 'false');
  git('remote', 'add', 'origin', 'https://github.com/dicode-ayo/e2e-fixture.git');
  git('add', '-A');
  git('commit', '-q', '-m', 'e2e fixture baseline');
}

function writeTaskset(tempDir: string): { tasksetPath: string; tasksDir: string } {
  const tasksDir = path.join(tempDir, 'tasks');
  copyDirSync(TASKS_DIR, tasksDir);
  initFixtureRepo(tasksDir);

  const buildinDir = ensureBuildinCheckout(REPO_ROOT);
  const buildinWebuiTaskYaml = path.join(buildinDir, 'webui/task.yaml');
  const buildinMcpTaskYaml = path.join(buildinDir, 'mcp/task.yaml');
  const buildinAuthProvidersTaskYaml = path.join(buildinDir, 'auth-providers/task.yaml');
  const buildinLocalStorageTaskYaml = path.join(buildinDir, 'local-storage/task.yaml');
  const buildinRunInputsCleanupTaskYaml = path.join(buildinDir, 'run-inputs-cleanup/task.yaml');
  const authOauthAppTaskYaml = path.join(REPO_ROOT, 'tasks/auth/_oauth-app/task.yaml');
  const template = fs.readFileSync(path.join(TASKS_DIR, 'taskset.yaml'), 'utf8');
  const content = template
    .replace(/FIXTURES_TASKS_DIR/g, tasksDir)
    .replace(/BUILDIN_WEBUI_TASK_YAML/g, buildinWebuiTaskYaml)
    .replace(/BUILDIN_MCP_TASK_YAML/g, buildinMcpTaskYaml)
    .replace(/BUILDIN_AUTH_PROVIDERS_TASK_YAML/g, buildinAuthProvidersTaskYaml)
    .replace(/BUILDIN_LOCAL_STORAGE_TASK_YAML/g, buildinLocalStorageTaskYaml)
    .replace(/BUILDIN_RUN_INPUTS_CLEANUP_TASK_YAML/g, buildinRunInputsCleanupTaskYaml)
    .replace(/AUTH_OAUTH_APP_TASK_YAML/g, authOauthAppTaskYaml);
  const tasksetPath = path.join(tempDir, 'taskset.yaml');
  fs.writeFileSync(tasksetPath, content, 'utf8');
  return { tasksetPath, tasksDir };
}

/**
 * Copy the add-source-tasks fixture into its own, brand-new temp directory
 * (a distinct fs.mkdtempSync root — NOT a subdirectory of the main tempDir)
 * and write its resolved taskset.yaml. Returns the path to that taskset.yaml.
 *
 * Why a fully separate root rather than a subdirectory of tempDir: tempDir IS
 * data_dir (see setup() below), and the main "e2e-tests" source's fsnotify
 * watch root is the directory containing ITS taskset.yaml — which is tempDir
 * itself. A subdirectory of tempDir would still be a distinct watch root from
 * tempDir, so it wouldn't literally recreate the issue #621 collision, but a
 * fully independent root is simpler to reason about and to tear down (its own
 * fs.rmSync, no risk of the two cleanups racing over shared ancestry) — see
 * teardown() below.
 *
 * This fixture is deliberately never referenced by tests/e2e/fixtures/dicode-
 * unauth.yaml|dicode-auth.yaml's spec.entries: the whole point is that no
 * source watches it until add-source.spec.ts's own test dynamically adds one
 * pointed here via the real "Add source" form.
 */
function writeAddSourceTaskset(): { tasksetPath: string; tempDir: string } {
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dicode-e2e-add-source-'));
  const tasksDir = path.join(tempDir, 'tasks');
  copyDirSync(ADD_SOURCE_FIXTURES_DIR, tasksDir);

  const template = fs.readFileSync(path.join(ADD_SOURCE_FIXTURES_DIR, 'taskset.yaml'), 'utf8');
  const content = template.replace(/ADD_SOURCE_FIXTURES_TASKS_DIR/g, tasksDir);
  const tasksetPath = path.join(tempDir, 'taskset.yaml');
  fs.writeFileSync(tasksetPath, content, 'utf8');
  return { tasksetPath, tempDir };
}

/**
 * Instantiate a config template (replacing TEMP_DATA_DIR and TEMP_TASKSET_PATH)
 * and write it to tempDir/dicode.yaml.
 *
 * When `overlay` is given and non-empty, the substituted template is parsed
 * as YAML, `overlay` is deep-merged on top (see deepMerge), and the result
 * is re-serialized — this is how startIsolatedDaemon() adds/overrides config
 * keys without every caller having to know the fixture's exact YAML shape.
 * Round-tripping through the YAML parser drops the template's comments, so
 * this path is skipped (kept as plain string substitution) when there is no
 * overlay to apply — i.e. for the shared setup()/teardown() daemon, whose
 * config is worth keeping human-readable in CI logs.
 */
function writeConfig(
  templateName: 'dicode-unauth.yaml' | 'dicode-auth.yaml',
  tempDir: string,
  tasksetPath: string,
  overlay?: Record<string, unknown>,
): string {
  const template = fs.readFileSync(path.join(FIXTURES_DIR, templateName), 'utf8');
  const substituted = template
    .replace(/TEMP_DATA_DIR/g, tempDir)
    .replace(/TEMP_TASKSET_PATH/g, tasksetPath);
  const content =
    overlay && Object.keys(overlay).length > 0
      ? dumpYAML(deepMerge(loadYAML(substituted), overlay))
      : substituted;
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

// ─── shared instance startup ───────────────────────────────────────────────────

type AuthMode = 'authenticated' | 'unauthenticated';

interface RunningInstance {
  child: ChildProcess;
  pid: number;
  baseURL: string;
  tempDir: string;
  tasksDir: string;
  tasksetPath: string;
  configPath: string;
}

/**
 * Builds the fixtures, writes dicode.yaml (with `overlay` merged in, if
 * given — see writeConfig), spawns the daemon on `port`, and waits for it to
 * answer /api/tasks. Shared by setup() (the one project-wide daemon, fixed
 * port, no overlay) and startIsolatedDaemon() (a free port, caller-supplied
 * overlay).
 */
async function startInstance(opts: {
  authMode: AuthMode;
  port: number;
  overlay?: Record<string, unknown>;
}): Promise<RunningInstance> {
  ensureBinary();

  const templateName: 'dicode-unauth.yaml' | 'dicode-auth.yaml' =
    opts.authMode === 'authenticated' ? 'dicode-auth.yaml' : 'dicode-unauth.yaml';

  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dicode-e2e-'));
  const { tasksetPath, tasksDir } = writeTaskset(tempDir);
  const configPath = writeConfig(templateName, tempDir, tasksetPath, opts.overlay);
  const baseURL = `http://localhost:${opts.port}`;

  console.log(`[e2e] Starting dicode (${opts.authMode}) on port ${opts.port}`);
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

  child.stdout?.on('data', (d: Buffer) => process.stdout.write(`[dicode:${opts.port}] ${d}`));
  child.stderr?.on('data', (d: Buffer) => process.stderr.write(`[dicode:${opts.port}] ${d}`));
  child.on('exit', (code) => {
    if (code !== null && code !== 0) {
      console.error(`[e2e] dicode (port ${opts.port}) exited unexpectedly with code ${code}`);
    }
  });

  if (!child.pid) {
    throw new Error('[e2e] Failed to start dicode process — no PID returned');
  }

  const instance: RunningInstance = { child, pid: child.pid, baseURL, tempDir, tasksDir, tasksetPath, configPath };

  // The process is already running at this point. If it never comes up (a
  // bad overlay value, a port collision, a crash), don't leave it and its
  // temp dir behind for the rest of the run — stop it before propagating
  // the error.
  try {
    await waitForReady(baseURL);
  } catch (err) {
    await stopInstance(instance);
    throw err;
  }

  return instance;
}

/**
 * Kills an instance's process and removes its temp dir. Waits for a graceful
 * exit before falling back to SIGKILL, then removes tempDir once the process
 * is confirmed gone (avoids deleting files still open under it).
 */
async function stopInstance(instance: RunningInstance): Promise<void> {
  if (instance.child.exitCode === null && instance.child.signalCode === null) {
    instance.child.kill('SIGTERM');
    try {
      await waitForProcessExit(instance.child);
    } catch {
      // SIGTERM didn't land in time — force it, and this time actually wait
      // for the exit event rather than assuming SIGKILL is immediate: the
      // OS doesn't always release open file handles (data.db, its -wal)
      // synchronously with the kill call, and rmSync below has no retry of
      // its own for that.
      instance.child.kill('SIGKILL');
      await waitForProcessExit(instance.child).catch(() => {});
    }
  }
  fs.rmSync(instance.tempDir, { recursive: true, force: true });
}

/**
 * Spins up a fully-fixtured dicode daemon of its own — see the module doc
 * comment for when to use this instead of editing the shared fixture.
 *
 * `overlay` is deep-merged (see deepMerge) over the base
 * dicode-unauth.yaml/dicode-auth.yaml template; nested keys not mentioned in
 * `overlay` (e.g. `defaults.run_inputs.storage_task`) are preserved. The
 * returned daemon listens on its own free port and has its own temp data
 * dir — nothing about it is visible to any other spec or to the shared
 * setup()/teardown() daemon. Call `.stop()` in the spec's `afterAll`.
 *
 * This does not seed a Playwright auth-state cookie (see writeAuthState) —
 * it's meant for API-only specs against an unauthenticated daemon. A spec
 * that needs a browser session against its own isolated daemon would need
 * its own writeAuthState-equivalent call; none exists yet because no current
 * caller needs one.
 */
export async function startIsolatedDaemon(
  options: { overlay?: Record<string, unknown>; authMode?: AuthMode } = {},
): Promise<IsolatedDaemon> {
  const port = await freePort();
  // server.port is forced last, after the caller's overlay, not the other
  // way around: baseURL/waitForReady below are built from this exact `port`
  // value, so an overlay.server.port winning the merge would make the
  // written config and the port this function actually polls/returns
  // disagree — the daemon binds where the config says, this keeps talking
  // to a different, unbound port, and waitForReady times out.
  const overlay = deepMerge(options.overlay ?? {}, { server: { port } }) as Record<string, unknown>;
  const instance = await startInstance({
    authMode: options.authMode ?? 'unauthenticated',
    port,
    overlay,
  });
  return {
    baseURL: instance.baseURL,
    tempDir: instance.tempDir,
    tasksDir: instance.tasksDir,
    tasksetPath: instance.tasksetPath,
    configPath: instance.configPath,
    stop: () => stopInstance(instance),
  };
}

// ─── exported functions ────────────────────────────────────────────────────────

export async function setup(): Promise<void> {
  const authMode: AuthMode =
    process.env.DICODE_AUTH_MODE === 'authenticated' ? 'authenticated' : 'unauthenticated';

  const instance = await startInstance({ authMode, port: PORT });
  const { tempDir, tasksDir, tasksetPath, configPath, pid } = instance;
  const { tasksetPath: addSourceTasksetPath, tempDir: addSourceTempDir } = writeAddSourceTaskset();
  console.log(`[e2e] Add-source fixture temp dir: ${addSourceTempDir}`);

  const state: E2EState = {
    pid,
    tempDir,
    configPath,
    tasksetPath,
    addSourceTempDir,
  };
  fs.writeFileSync(STATE_FILE, JSON.stringify(state), 'utf8');

  // Expose paths to test files via environment variables.
  process.env.DICODE_E2E_TEMP_DIR = tempDir;
  process.env.DICODE_E2E_TASKSET_PATH = tasksetPath;
  process.env.DICODE_E2E_CONFIG_PATH = configPath;
  process.env.DICODE_E2E_TASKS_DIR = tasksDir;
  process.env.DICODE_E2E_ADD_SOURCE_TASKSET_PATH = addSourceTasksetPath;

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
  // Separate root from tempDir (see writeAddSourceTaskset) — cleaned up
  // independently since it's never nested under tempDir.
  if (state.addSourceTempDir && fs.existsSync(state.addSourceTempDir)) {
    fs.rmSync(state.addSourceTempDir, { recursive: true, force: true });
  }
  fs.rmSync(STATE_FILE, { force: true });
  fs.rmSync(AUTH_STATE_PATH, { force: true });
  console.log('[e2e] Cleanup complete.');
}
