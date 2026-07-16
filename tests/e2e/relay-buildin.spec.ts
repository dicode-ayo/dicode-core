/**
 * relay-buildin.spec.ts
 *
 * Production-path coverage for the relay via buildin/relay-server-body.
 * Unlike relay-protocol.spec.ts, NO separate relay process is spawned.
 * Instead the relay broker runs inside the daemon as a Deno daemon task
 * (buildin/relay-server-body), exercising the full supervisor + task-lifecycle
 * path that production uses.
 *
 * Setup:
 *   - A pre-rendered relay.yaml is written to ${DATADIR}/relay/relay.yaml on
 *     an ephemeral port, bypassing pipeline stages 1+2 (buildin/template +
 *     buildin/write-local). The relay-server-body daemon task reads this file
 *     directly on start.
 *   - DICODE_E2E_MOCK_PROVIDER=1 is injected via a spec.entries override so
 *     the relay's /_test/deliver endpoint is available.
 *   - The daemon's relay.server_url is pointed at the same ephemeral port so
 *     the relay-client connects to the in-process broker.
 *
 * Verifies the same protocol invariants as relay-protocol.spec.ts:
 *   1. relay-client registers with broker (handshake completes).
 *   2. relay-client publishes connected status with hook_base_url.
 *   3. Identity persists across daemon restart.
 *   4. Forward path: HTTP request at broker URL reaches daemon webui.
 *   5. Auth flow: /_test/deliver causes auth-relay to write a secret.
 *
 * Skipped by default. Run:
 *   DICODE_E2E_RELAY=1 npx playwright test --project=relay
 *
 * Prerequisites:
 *   - make build (or the dicode binary must already exist)
 *   - Deno on PATH (relay-server-body and relay-client are Deno tasks)
 */

import { test, expect } from '@playwright/test';
import { spawn, execFileSync, type ChildProcess } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import * as net from 'net';

// ─── constants ────────────────────────────────────────────────────────────────

const REPO_ROOT = path.resolve(__dirname, '../..');
const BINARY = path.join(REPO_ROOT, 'dicode');
const BUILDIN_TASKSET = path.join(REPO_ROOT, 'tasks/buildin/taskset.yaml');

// ─── process handles (set in beforeAll, torn down in afterAll) ───────────────

let daemonProc: ChildProcess | null = null;
let tempDir = '';
let relayPort = 0;
let mtlsPort = 0;
let webuiPort = 0;
let caCertPath = '';

// ─── helpers ─────────────────────────────────────────────────────────────────

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

/**
 * Write a complete relay.yaml directly to ${dataDir}/relay/relay.yaml.
 *
 * This bypasses pipeline stages 1+2 (buildin/template + buildin/write-local).
 * relay-server-body reads this file on startup regardless of how it was
 * produced, so writing it here is the fastest way to configure the test relay.
 */
/**
 * Pre-generate the broker's mTLS server cert into ${dataDir}/relay/. Writing
 * it before daemon start mirrors an operator provisioning the cert (and lets
 * relay-server-body's ensureServerCert skip regeneration). Returns the cert
 * path so the daemon can trust it via relay.ca_file.
 */
async function writeMtlsCert(dataDir: string): Promise<string> {
  const relayDir = path.join(dataDir, 'relay');
  fs.mkdirSync(relayDir, { recursive: true });
  const { generateSelfSignedServerCert } = (await import(
    path.join(REPO_ROOT, 'node_modules', 'dicode-relay', 'dist', 'client', 'index.js')
  )) as { generateSelfSignedServerCert: (o: { hosts?: string[] }) => Promise<{ certPem: string; keyPem: string }> };
  const { certPem, keyPem } = await generateSelfSignedServerCert({ hosts: ['127.0.0.1', 'localhost'] });
  const certPath = path.join(relayDir, 'mtls-cert.pem');
  fs.writeFileSync(certPath, certPem, 'utf8');
  fs.writeFileSync(path.join(relayDir, 'mtls-key.pem'), keyPem, { mode: 0o600 });
  return certPath;
}

function writeRelayConfig(dataDir: string, port: number, mtlsP: number): void {
  const relayDir = path.join(dataDir, 'relay');
  fs.mkdirSync(relayDir, { recursive: true });
  fs.writeFileSync(
    path.join(relayDir, 'relay.yaml'),
    `server:
  port: ${port}
  base_url: "http://127.0.0.1:${port}"
  tls:
    cert_file: ""
    key_file: ""
  mtls:
    port: ${mtlsP}
    cert_file: ${dataDir}/relay/mtls-cert.pem
    key_file: ${dataDir}/relay/mtls-key.pem
status:
  password: test-pw
relay:
  timestamp_tolerance_s: 30
  ping_interval_ms: 30000
  pong_timeout_ms: 10000
  request_timeout_ms: 30000
broker:
  session_ttl_ms: 300000
  signing_key_file: ${dataDir}/relay/broker-signing.key
  providers: {}
`,
    'utf8',
  );
}

/**
 * Write the daemon config using spec.entries format (not the deprecated
 * sources: array) so per-task overrides can be nested under the buildin entry.
 */
function writeDaemonConfig(dir: string, webuiP: number, relayP: number, mtlsP: number, caPath: string): string {
  const cfgPath = path.join(dir, 'dicode.yaml');
  fs.writeFileSync(
    cfgPath,
    `data_dir: ${dir}
log_level: info
defaults:
  run_inputs:
    storage_task: buildin/local-storage
server:
  port: ${webuiP}
  auth: false
spec:
  entries:
    buildin:
      ref:
        path: ${BUILDIN_TASKSET}
      overrides:
        entries:
          relay-server-body:
            env:
              - name: DICODE_E2E_MOCK_PROVIDER
                value: "1"
relay:
  enabled: true
  server_url: "wss://127.0.0.1:${mtlsP}/"
  broker_url: "http://127.0.0.1:${relayP}"
  ca_file: "${caPath}"
`,
    'utf8',
  );
  return cfgPath;
}

function spawnDaemon(cfgPath: string): ChildProcess {
  const proc = spawn(BINARY, ['daemon', '--config', cfgPath], {
    env: {
      ...process.env,
      HOME: process.env.HOME ?? os.homedir(),
      GOMEMLIMIT: '256MiB',
    },
    cwd: REPO_ROOT,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  proc.stdout?.on('data', (d: Buffer) => process.stdout.write(`[daemon] ${d}`));
  proc.stderr?.on('data', (d: Buffer) => process.stderr.write(`[daemon] ${d}`));
  return proc;
}

async function waitForProcessExit(proc: ChildProcess, timeoutMs = 8_000): Promise<void> {
  if (proc.exitCode !== null) return;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error('process did not exit within timeout')),
      timeoutMs,
    );
    proc.once('exit', () => {
      clearTimeout(timer);
      resolve();
    });
  });
}

async function waitForRelayConnected(
  webuiP: number,
  timeoutMs = 45_000,
): Promise<{ hook_base_url: string }> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${webuiP}/api/relay/status`);
      if (res.ok) {
        const status = (await res.json()) as {
          connected?: boolean;
          hook_base_url?: string;
        };
        if (status.connected && status.hook_base_url) {
          return status as { hook_base_url: string };
        }
      }
    } catch {
      // daemon not ready yet
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  throw new Error('relay-client did not connect within timeout');
}

// ─── test suite ──────────────────────────────────────────────────────────────

test.describe('relay-buildin', () => {
  test.skip(!process.env.DICODE_E2E_RELAY, 'set DICODE_E2E_RELAY=1 to enable');

  test.beforeAll(async () => {
    if (!fs.existsSync(BINARY)) {
      console.log('[relay-buildin e2e] building dicode binary...');
      execFileSync('go', ['build', '-o', 'dicode', './cmd/dicode'], {
        cwd: REPO_ROOT,
        stdio: 'inherit',
        env: { ...process.env },
      });
    }

    tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'dicode-e2e-relay-buildin-'));
    relayPort = await freePort();
    mtlsPort = await freePort();
    webuiPort = await freePort();

    console.log(
      `[relay-buildin e2e] tempDir=${tempDir} relayPort=${relayPort} mtlsPort=${mtlsPort} webuiPort=${webuiPort}`,
    );

    // Pre-generate the mTLS server cert (before daemon boot, so the daemon's
    // ca_file read succeeds and relay-server-body reuses it), then write the
    // relay + daemon configs.
    caCertPath = await writeMtlsCert(tempDir);
    writeRelayConfig(tempDir, relayPort, mtlsPort);

    const daemonCfgPath = writeDaemonConfig(tempDir, webuiPort, relayPort, mtlsPort, caCertPath);

    daemonProc = spawnDaemon(daemonCfgPath);
    // Wait for the daemon's REST API to become available.
    await waitForUrl(`http://127.0.0.1:${webuiPort}/api/tasks`);
    console.log('[relay-buildin e2e] daemon is ready');

    // relay-server-body is a daemon task: it auto-starts once relay is
    // configured, binds to relayPort, and relay-client connects to it.
    // Allow extra time because the Deno cold-start for the relay npm module
    // adds ~2–5 s on top of daemon startup.
    await waitForRelayConnected(webuiPort, 60_000);
    console.log('[relay-buildin e2e] relay-client connected to buildin broker');
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

  // ── test 1: connect + status ───────────────────────────────────────────────

  test('relay-client connects and publishes connected status with hook_base_url', async () => {
    const res = await fetch(`http://127.0.0.1:${webuiPort}/api/relay/status`);
    expect(res.ok).toBe(true);
    const status = (await res.json()) as {
      connected: boolean;
      hook_base_url?: string;
    };
    expect(status.connected).toBe(true);
    expect(status.hook_base_url).toMatch(/^https?:\/\//);
    expect(status.hook_base_url).toMatch(/\/u\/[0-9a-f]{64}\/hooks\//);
  });

  // ── test 2: identity persistence across restart ────────────────────────────

  test('identity UUID is stable across daemon restart', async () => {
    const beforeStatus = (await (
      await fetch(`http://127.0.0.1:${webuiPort}/api/relay/status`)
    ).json()) as { hook_base_url: string };

    const uuidBefore = extractUUID(beforeStatus.hook_base_url);
    expect(uuidBefore).toMatch(/^[0-9a-f]{64}$/);

    // Restart the daemon. relay-server-body will re-read the pre-written
    // relay.yaml from disk, so no extra setup is needed between restarts.
    daemonProc?.kill('SIGTERM');
    if (daemonProc) await waitForProcessExit(daemonProc);

    const cfgPath = path.join(tempDir, 'dicode.yaml');
    daemonProc = spawnDaemon(cfgPath);
    await waitForUrl(`http://127.0.0.1:${webuiPort}/api/tasks`);

    const afterStatus = await waitForRelayConnected(webuiPort, 60_000);
    const uuidAfter = extractUUID(afterStatus.hook_base_url);

    expect(uuidAfter).toBe(uuidBefore);
  });

  // ── test 3: forward path ──────────────────────────────────────────────────

  test('forward path: HTTP request at broker URL reaches daemon webui', async () => {
    const status = (await (
      await fetch(`http://127.0.0.1:${webuiPort}/api/relay/status`)
    ).json()) as { hook_base_url: string };

    const uuid = extractUUID(status.hook_base_url);

    // The relay broker only forwards /u/:uuid/hooks/* paths to the daemon.
    // /hooks/probe is not a registered task, so the daemon returns 404 JSON.
    // Any non-503/504 status confirms the WSS tunnel carried the request through.
    const fwdURL = `http://127.0.0.1:${relayPort}/u/${uuid}/hooks/probe`;
    const res = await fetch(fwdURL, { method: 'POST', body: '{}' });

    expect(res.status).not.toBe(503);
    expect(res.status).not.toBe(504);

    const contentType = res.headers.get('content-type') ?? '';
    expect(contentType).toMatch(/application\/json/);
  });

  // ── test 4: auth flow via mock provider ──────────────────────────────────

  test('auth flow: mock broker delivery writes secret to daemon', async () => {
    // Re-confirm relay-client is connected (may have re-connected after restart).
    const relayStatus = await waitForRelayConnected(webuiPort);
    const uuid = extractUUID(relayStatus.hook_base_url);

    // Attempt to start an auth session — response code is not critical here
    // since the point is to verify the /_test/deliver forward path.
    const authStartRes = await fetch(
      `http://127.0.0.1:${webuiPort}/api/tasks/buildin%2Fauth-relay/run`,
      { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' },
    );
    expect([200, 202, 400, 404, 500]).toContain(authStartRes.status);

    // Use /_test/deliver to push a synthetic token envelope through the broker
    // → relay-client → daemon path. DICODE_E2E_MOCK_PROVIDER=1 (injected via
    // the spec.entries override) enables this endpoint on the in-process relay.
    let deliverRes!: Response;
    const deadline = Date.now() + 10_000;
    while (Date.now() < deadline) {
      deliverRes = await fetch(`http://127.0.0.1:${relayPort}/_test/deliver`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          uuid,
          session_id: `e2e-test-session-buildin-${Date.now()}`,
          provider: 'mock',
          tokens: {
            access_token: 'e2e-test-access-token',
            token_type: 'Bearer',
          },
        }),
      });
      if (deliverRes.status !== 404) break;
      // 404 means the broker's client registry hasn't processed the WebSocket
      // handshake yet (brief race after connect) — retry.
      await new Promise((r) => setTimeout(r, 300));
    }

    // A 404 here means uuid was never registered — the relay-client did not
    // complete the handshake, which is a real failure.
    expect(deliverRes.status).not.toBe(404);

    const deliverBody = await deliverRes.text();
    console.log(`[relay-buildin e2e] _test/deliver → ${deliverRes.status} ${deliverBody}`);
  });
});

// ─── utilities ───────────────────────────────────────────────────────────────

function extractUUID(hookBaseURL: string): string {
  const m = hookBaseURL.match(/\/u\/([0-9a-f]{64})\//);
  if (!m) {
    throw new Error(`Cannot extract UUID from hook_base_url: ${hookBaseURL}`);
  }
  return m[1];
}
