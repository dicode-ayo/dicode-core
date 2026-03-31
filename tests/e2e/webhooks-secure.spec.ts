/**
 * webhooks-secure.spec.ts
 *
 * Tests for HMAC-authenticated webhook endpoints.
 *
 * The test task hello-webhook-secure has:
 *   webhook_secret: "${TEST_WEBHOOK_SECRET}"
 *
 * The dicode server is started by global setup with TEST_WEBHOOK_SECRET in its
 * environment, so the variable is expanded at task load time.
 *
 * If TEST_WEBHOOK_SECRET is not set these tests are skipped gracefully.
 */

import { test, expect } from '@playwright/test';
import * as crypto from 'crypto';

const SECURE_WEBHOOK_PATH = '/hooks/test-webhook-secure';
const SECURE_TASK_ID = 'e2e-tests/hello-webhook-secure';

const TEST_SECRET = process.env.TEST_WEBHOOK_SECRET ?? 'e2e-test-webhook-secret-xyz';

/** Compute sha256 HMAC over body using secret */
function sign(secret: string, body: string): string {
  return 'sha256=' + crypto.createHmac('sha256', secret).update(body).digest('hex');
}

test.describe('Secure Webhook (HMAC)', () => {
  // Skip the whole suite if no secret is configured on the server.
  // We use the TEST_WEBHOOK_SECRET env var — if it wasn't passed to the server
  // at startup the task's webhook_secret will remain literal "${TEST_WEBHOOK_SECRET}",
  // which means every request would need to present that as the HMAC key.
  // In the default CI flow TEST_WEBHOOK_SECRET is set to a known value.

  test('POST without signature header → 403', async ({ request }) => {
    const body = JSON.stringify({ test: 'no-sig' });
    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: JSON.parse(body),
    });
    // Must be 403 — missing signature
    expect(res.status()).toBe(403);
  });

  test('POST with wrong signature → 403', async ({ request }) => {
    const body = JSON.stringify({ test: 'bad-sig' });
    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': 'sha256=deadbeefdeadbeefdeadbeef',
      },
      data: JSON.parse(body),
    });
    expect(res.status()).toBe(403);
  });

  test('POST with correct signature → 200', async ({ request }) => {
    const bodyStr = JSON.stringify({ test: 'valid-sig', ts: Date.now() });
    const sig = sign(TEST_SECRET, bodyStr);

    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': sig,
      },
      data: JSON.parse(bodyStr),
    });
    expect(res.ok()).toBe(true);
  });

  test('signed request sets X-Run-Id header', async ({ request }) => {
    const bodyStr = JSON.stringify({ check: 'run-id' });
    const sig = sign(TEST_SECRET, bodyStr);

    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': sig,
      },
      data: JSON.parse(bodyStr),
    });
    expect(res.ok()).toBe(true);
    const runId = res.headers()['x-run-id'];
    expect(runId).toBeTruthy();
  });

  test('signed webhook run completes successfully', async ({ request }) => {
    const bodyStr = JSON.stringify({ msg: 'complete' });
    const sig = sign(TEST_SECRET, bodyStr);

    const res = await request.post(`${SECURE_WEBHOOK_PATH}?wait=false`, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': sig,
      },
      data: JSON.parse(bodyStr),
    });
    expect(res.ok()).toBe(true);
    const { runId } = await res.json() as { runId: string };

    // Poll for completion.
    const deadline = Date.now() + 30_000;
    let finalStatus = '';
    while (Date.now() < deadline) {
      const r = await request.get(`/api/runs/${runId}`);
      if (r.ok()) {
        const b = await r.json() as { status?: string; Status?: string };
        const s = b.status || b.Status;
        if (s && s !== 'running') { finalStatus = s; break; }
      }
      await new Promise((r2) => setTimeout(r2, 500));
    }
    expect(finalStatus).toBe('success');
  });

  test('signed request result contains input payload', async ({ request }) => {
    const payload = { from: 'hmac-test', value: 123 };
    const bodyStr = JSON.stringify(payload);
    const sig = sign(TEST_SECRET, bodyStr);

    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': sig,
      },
      data: payload,
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as Record<string, unknown>;
    expect(body).toHaveProperty('received');
    const received = body.received as Record<string, unknown>;
    expect(received.from).toBe('hmac-test');
  });

  test('signature on wrong body → 403', async ({ request }) => {
    const realBody = JSON.stringify({ real: 'body' });
    const fakeBody = JSON.stringify({ tampered: 'body' });
    // Sign the real body but send the tampered body.
    const sig = sign(TEST_SECRET, realBody);

    const res = await request.post(SECURE_WEBHOOK_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': sig,
      },
      data: JSON.parse(fakeBody),
    });
    expect(res.status()).toBe(403);
  });
});
