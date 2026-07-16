/**
 * webhook-auth-any.spec.ts
 *
 * End-to-end coverage for trigger.auth: "any" (core#610) — a webhook that
 * authenticates via EITHER a valid dicode session OR a valid HMAC signature.
 *
 * Runs in the 'authenticated' project (server.auth: true, passphrase
 * test-passphrase-12345) so both credentials are exercisable against one
 * fixture: hello-webhook-any, which declares
 *   trigger: { auth: any, webhook_secret: "${TEST_WEBHOOK_SECRET}" }
 *
 * The server is started with TEST_WEBHOOK_SECRET in its env, so the secret is
 * expanded at task load time. These tests are meaningful only when that env var
 * is set (it is in the CI e2e job).
 */

import { test, expect } from '@playwright/test';
import * as crypto from 'crypto';
import { TEST_PASSPHRASE, login } from './helpers/auth';

const ANY_WEBHOOK_PATH = '/hooks/test-webhook-any';
const TEST_SECRET = process.env.TEST_WEBHOOK_SECRET ?? 'test-webhook-secret-12345';

// A syntactically-valid but non-relayed relay base — the guard only checks for
// the header's presence to classify a request as relayed.
const RELAY_BASE = '/u/' + '0'.repeat(64);

/** Compute the GitHub-style sha256 HMAC header value over body. */
function sign(secret: string, body: string): string {
  return 'sha256=' + crypto.createHmac('sha256', secret).update(body).digest('hex');
}

/** HMAC over "<ts>\n<body>" — the timestamp-bound preimage (#605). */
function signWithTs(secret: string, ts: string, body: string): string {
  return 'sha256=' + crypto.createHmac('sha256', secret).update(ts + '\n' + body).digest('hex');
}

const DOWNGRADE_PATH = '/hooks/test-webhook-downgrade';
const TS_PATH = '/hooks/test-webhook-ts';

test.describe('Webhook auth: any (session OR HMAC)', () => {
  // ── HMAC (machine caller, no session) ──────────────────────────────────────
  test('POST with valid HMAC signature, no session → 200', async ({ request }) => {
    const bodyStr = JSON.stringify({ via: 'hmac', ts: Date.now() });
    const res = await request.post(ANY_WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': sign(TEST_SECRET, bodyStr) },
      data: JSON.parse(bodyStr),
    });
    expect(res.ok()).toBe(true);
    expect(res.headers()['x-run-id']).toBeTruthy();
  });

  test('POST without signature, no session → 403', async ({ request }) => {
    const res = await request.post(ANY_WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: { via: 'nothing' },
    });
    expect(res.status()).toBe(403);
  });

  test('POST with wrong signature, no session → 403', async ({ request }) => {
    const res = await request.post(ANY_WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json', 'X-Hub-Signature-256': 'sha256=deadbeef' },
      data: { via: 'bad-sig' },
    });
    expect(res.status()).toBe(403);
  });

  // ── Session (browser, no signature) ────────────────────────────────────────
  test('POST with a valid session and NO signature → 200 (HMAC skipped)', async ({ request }) => {
    await login(request, TEST_PASSPHRASE); // populates the context cookie jar
    const res = await request.post(ANY_WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: { via: 'session', ts: Date.now() },
    });
    expect(res.ok()).toBe(true);
  });

  test('session request repeats an identical body without a 409 (replay skipped)', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const identical = { via: 'session-dup', fixed: 'body' };
    for (let i = 0; i < 2; i++) {
      const res = await request.post(ANY_WEBHOOK_PATH, {
        headers: { 'Content-Type': 'application/json' },
        data: identical,
      });
      expect(res.ok(), `submission ${i}`).toBe(true);
    }
  });

  // ── GET must never fall through to HMAC (trap 1) ───────────────────────────
  test('GET without a session is not served (auth-gated UI stays private)', async ({ request }) => {
    const res = await request.get(ANY_WEBHOOK_PATH, {
      headers: { Accept: 'text/html' },
      maxRedirects: 0,
    });
    // Browser GET → 303 login redirect; never 200 (which would serve/run it).
    expect(res.status()).not.toBe(200);
    expect([303, 401]).toContain(res.status());
  });

  // ── Relay (only HMAC crosses it) ───────────────────────────────────────────
  test('relayed POST with valid HMAC → 200', async ({ request }) => {
    const bodyStr = JSON.stringify({ via: 'relay-hmac', ts: Date.now() });
    const res = await request.post(ANY_WEBHOOK_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': sign(TEST_SECRET, bodyStr),
        'X-Relay-Base': RELAY_BASE,
      },
      data: JSON.parse(bodyStr),
    });
    expect(res.ok()).toBe(true);
  });

  test('relayed POST without signature → 403', async ({ request }) => {
    const res = await request.post(ANY_WEBHOOK_PATH, {
      headers: { 'Content-Type': 'application/json', 'X-Relay-Base': RELAY_BASE },
      data: { via: 'relay-nosig' },
    });
    expect(res.status()).toBe(403);
  });

  // ── Static UI assets stay session-gated (asset fall-through fix) ───────────
  test('unsigned POST to a static asset sub-path is not served', async ({ request }) => {
    const res = await request.post(`${ANY_WEBHOOK_PATH}/app.js`, {
      headers: { 'Content-Type': 'application/json' },
      data: { via: 'asset-probe' },
    });
    expect(res.status()).not.toBe(200);
    expect(await res.text()).not.toContain('hello-webhook-any-asset-marker');
  });

  test('GET to a static asset sub-path without a session is not served', async ({ request }) => {
    const res = await request.get(`${ANY_WEBHOOK_PATH}/app.js`, {
      headers: { Accept: 'text/html' },
      maxRedirects: 0,
    });
    expect(res.status()).not.toBe(200);
    expect(await res.text()).not.toContain('hello-webhook-any-asset-marker');
  });

  test('relayed browser GET → 401 HTML explainer, not a login bounce', async ({ request }) => {
    const res = await request.get(ANY_WEBHOOK_PATH, {
      headers: { Accept: 'text/html', 'X-Relay-Base': RELAY_BASE },
      maxRedirects: 0,
    });
    expect(res.status()).toBe(401);
    expect(res.headers()['content-type'] ?? '').toContain('text/html');
    expect(res.headers()['location'] ?? '').toBe('');
  });
});

// core#611 safety guard: an auth: any webhook whose secret env var is unset
// must degrade to session-only, never serving the unresolved ${VAR} placeholder
// as a live HMAC key. The fixture references DICODE_E2E_DOWNGRADE_UNSET, which
// the harness never sets.
test.describe('Webhook auth: any downgrade (no resolved secret)', () => {
  test('unsigned POST, no session → denied (not run via HMAC)', async ({ request }) => {
    const res = await request.post(DOWNGRADE_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: { via: 'unsigned' },
      maxRedirects: 0,
    });
    // Downgraded to session: a no-session POST is rejected (401), never 200.
    expect(res.status()).not.toBe(200);
    expect([401, 303]).toContain(res.status());
  });

  test('POST signed with the placeholder literal → denied (placeholder is not a key)', async ({ request }) => {
    // An attacker who read the committed task.yaml signs with the public literal.
    const body = JSON.stringify({ via: 'placeholder-attack' });
    const res = await request.post(DOWNGRADE_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': sign('${DICODE_E2E_DOWNGRADE_UNSET}', body),
      },
      data: JSON.parse(body),
      maxRedirects: 0,
    });
    // The webhook is session-only now; the signature is irrelevant. Never 200.
    expect(res.status()).not.toBe(200);
    expect([401, 303]).toContain(res.status());
  });

  test('POST with a valid session → 200 (session path still works)', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.post(DOWNGRADE_PATH, {
      headers: { 'Content-Type': 'application/json' },
      data: { via: 'session' },
    });
    expect(res.ok()).toBe(true);
  });
});

// core#605: require_timestamp gate + timestamp-bound replay cache.
test.describe('Webhook auth: any + require_timestamp', () => {
  test('signed with a valid timestamp → 200', async ({ request }) => {
    const ts = String(Math.floor(Date.now() / 1000));
    const body = JSON.stringify({ via: 'ts', n: Date.now() });
    const res = await request.post(TS_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Dicode-Timestamp': ts,
        'X-Hub-Signature-256': signWithTs(TEST_SECRET, ts, body),
      },
      data: JSON.parse(body),
    });
    expect(res.ok()).toBe(true);
  });

  test('body-only signature, no timestamp → 403 (require_timestamp)', async ({ request }) => {
    const body = JSON.stringify({ via: 'no-ts' });
    const res = await request.post(TS_PATH, {
      headers: {
        'Content-Type': 'application/json',
        'X-Hub-Signature-256': sign(TEST_SECRET, body), // valid body-only sig, but no timestamp
      },
      data: JSON.parse(body),
    });
    expect(res.status()).toBe(403);
  });

  test('exact (timestamp, body) replay → second is rejected', async ({ request }) => {
    const ts = String(Math.floor(Date.now() / 1000));
    const body = JSON.stringify({ via: 'replay', fixed: true });
    const headers = {
      'Content-Type': 'application/json',
      'X-Dicode-Timestamp': ts,
      'X-Hub-Signature-256': signWithTs(TEST_SECRET, ts, body),
    };
    const first = await request.post(TS_PATH, { headers, data: JSON.parse(body) });
    expect(first.ok()).toBe(true);
    const second = await request.post(TS_PATH, { headers, data: JSON.parse(body) });
    expect(second.status()).toBe(409); // duplicate (timestamp, body)
  });

  test('same body, distinct timestamps → both accepted (no false collision)', async ({ request }) => {
    const body = JSON.stringify({ via: 'distinct-ts', fixed: true });
    const ts1 = String(Math.floor(Date.now() / 1000));
    const ts2 = String(Math.floor(Date.now() / 1000) + 1);
    const r1 = await request.post(TS_PATH, {
      headers: { 'Content-Type': 'application/json', 'X-Dicode-Timestamp': ts1, 'X-Hub-Signature-256': signWithTs(TEST_SECRET, ts1, body) },
      data: JSON.parse(body),
    });
    expect(r1.ok()).toBe(true);
    const r2 = await request.post(TS_PATH, {
      headers: { 'Content-Type': 'application/json', 'X-Dicode-Timestamp': ts2, 'X-Hub-Signature-256': signWithTs(TEST_SECRET, ts2, body) },
      data: JSON.parse(body),
    });
    expect(r2.ok(), 'distinct timestamp over identical body must not be a replay').toBe(true);
  });
});
