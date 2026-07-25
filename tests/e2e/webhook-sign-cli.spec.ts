/**
 * webhook-sign-cli.spec.ts
 *
 * End-to-end coverage for `dicode webhook sign`: the CLI's HMAC output is fed
 * straight into a POST against a real running dicode daemon's protected
 * webhook endpoint (hello-webhook-secure, see webhooks-secure.spec.ts) and
 * must be accepted — this is the "would have failed before this change" case,
 * since the `webhook sign` subcommand did not exist before issue #606.
 *
 * The CLI itself needs no daemon (pure local HMAC, handled before
 * ensureDaemon in cmd/dicode/main.go) — only the assertion step talks to the
 * e2e server.
 *
 * TEST_WEBHOOK_SECRET mirrors webhooks-secure.spec.ts: if it wasn't passed to
 * the server at startup, the task's webhook_secret stays the literal
 * "${TEST_WEBHOOK_SECRET}" string and every request needs to present that as
 * the HMAC key. In the default CI flow (make test-e2e*) it is always set.
 */

import { test, expect } from '@playwright/test';
import { execFileSync } from 'child_process';
import { BINARY } from './helpers/dicode-server';

const SECURE_WEBHOOK_PATH = '/hooks/test-webhook-secure';
const TEST_SECRET = process.env.TEST_WEBHOOK_SECRET ?? 'e2e-test-webhook-secret-xyz';

/**
 * Runs the real `dicode webhook sign` CLI and parses the two header lines out
 * of its stdout. Deliberately does NOT recompute the HMAC itself — that would
 * only prove the test's own crypto, not the CLI's.
 */
function signWithCli(args: string[]): { signature: string; timestamp: string | null } {
  const out = execFileSync(BINARY, ['webhook', 'sign', ...args], { encoding: 'utf8' });
  let signature = '';
  let timestamp: string | null = null;
  for (const line of out.trim().split('\n')) {
    if (!line) continue;
    const idx = line.indexOf(': ');
    if (idx === -1) continue;
    const name = line.slice(0, idx);
    const value = line.slice(idx + 2);
    if (name === 'X-Hub-Signature-256') signature = value;
    if (name === 'X-Dicode-Timestamp') timestamp = value;
  }
  return { signature, timestamp };
}

test.describe('dicode webhook sign (CLI) against a real protected webhook', () => {
  test('CLI-signed request with a timestamp is accepted', async ({ request }) => {
    const bodyStr = JSON.stringify({ via: 'cli', mode: 'timestamped' });
    const { signature, timestamp } = signWithCli(['--secret', TEST_SECRET, '--data', bodyStr]);

    expect(signature).toMatch(/^sha256=[0-9a-f]{64}$/);
    expect(timestamp).toBeTruthy();

    const headers: Record<string, string> = { 'Content-Type': 'application/json', 'X-Hub-Signature-256': signature };
    if (timestamp) headers['X-Dicode-Timestamp'] = timestamp;

    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers,
      data: JSON.parse(bodyStr),
    });
    expect(res.ok(), `expected 2xx, got ${res.status()}: ${await res.text()}`).toBe(true);
  });

  test('CLI --no-timestamp produces a GitHub-compatible bare-body signature that is accepted', async ({ request }) => {
    const bodyStr = JSON.stringify({ via: 'cli', mode: 'bare-body' });
    const { signature, timestamp } = signWithCli(['--secret', TEST_SECRET, '--data', bodyStr, '--no-timestamp']);

    expect(signature).toMatch(/^sha256=[0-9a-f]{64}$/);
    expect(timestamp).toBeNull();

    // No X-Dicode-Timestamp header is sent at all — bare-body mode.
    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': signature },
      data: JSON.parse(bodyStr),
    });
    expect(res.ok(), `expected 2xx, got ${res.status()}: ${await res.text()}`).toBe(true);
  });

  test('CLI-signed request with the wrong secret is rejected (403)', async ({ request }) => {
    const bodyStr = JSON.stringify({ via: 'cli', mode: 'wrong-secret' });
    const { signature } = signWithCli(['--secret', TEST_SECRET + '-WRONG', '--data', bodyStr, '--no-timestamp']);

    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': signature },
      data: JSON.parse(bodyStr),
    });
    expect(res.status()).toBe(403);
  });
});
