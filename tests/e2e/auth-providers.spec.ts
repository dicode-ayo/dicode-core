/**
 * auth-providers.spec.ts — e2e tests for the buildin/auth-providers
 * dashboard task.
 */
import { test, expect } from '@playwright/test';
import { TEST_PASSPHRASE, login } from './helpers/auth';

test.describe('Auth Providers dashboard', () => {

  test('GET /hooks/auth-providers serves index.html with SDK injected', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.get('/hooks/auth-providers');
    expect(res.ok()).toBe(true);
    const html = await res.text();
    expect(html).toContain('<title>Auth Providers');
    // The dicode.js SDK is auto-injected by the trigger engine.
    expect(html.toLowerCase()).toContain('dicode');
  });

  test('list action returns provider rows with has_token false by default', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    // The trigger engine serves index.html on GET when one is present, so
    // we invoke the list action over POST instead. The task reads
    // input.action from any method.
    const res = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'list' },
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as { result: Array<Record<string, unknown>> } | Array<Record<string, unknown>>;
    const rows = Array.isArray(body) ? body : body.result;
    expect(Array.isArray(rows)).toBe(true);
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row.has_token).toBe(false);
    }
    // Standalone openrouter is always listed (no broker required for it).
    const openrouter = rows.find(r => r.key === 'openrouter');
    expect(openrouter).toBeDefined();
  });

  test('list returns the post-#256 PublicProviderInfo shape', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'list' },
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as { result: Array<Record<string, unknown>> } | Array<Record<string, unknown>>;
    const rows = Array.isArray(body) ? body : body.result;
    const openrouter = rows.find(r => r.key === 'openrouter');
    // Standalone PKCE provider — the canonical fields the dashboard reads.
    expect(openrouter).toMatchObject({
      key: 'openrouter',
      pkce: true,
      secret_required: false,
      configured: true,
      has_token: false,
    });
    expect(Array.isArray(openrouter?.scopes)).toBe(true);
    expect(openrouter?.standalone).toMatchObject({ webhookPath: '/hooks/openrouter-oauth' });
  });

  test('list reports has_token=true when an ACCESS_TOKEN secret is set', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const setRes = await request.post('/api/secrets', {
      headers: { 'Content-Type': 'application/json' },
      data: { key: 'OPENROUTER_ACCESS_TOKEN', value: 'sk-or-test-12345' },
    });
    expect(setRes.ok()).toBe(true);

    const listRes = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'list' },
    });
    expect(listRes.ok()).toBe(true);
    const body = await listRes.json() as { result: Array<Record<string, unknown>> } | Array<Record<string, unknown>>;
    const rows = Array.isArray(body) ? body : body.result;
    const openrouter = rows.find(r => r.key === 'openrouter');
    expect(openrouter?.has_token).toBe(true);

    // Cleanup so subsequent tests are not polluted.
    await request.delete('/api/secrets/OPENROUTER_ACCESS_TOKEN');
  });

  test('list returns standalones only when relay is disabled (BYO without relay)', async ({ request }) => {
    // The e2e fixture does not enable relay, so DICODE_RELAY_BROKER_URL is
    // unset in the daemon environment. The task should fall back to the
    // STANDALONE catalogue rather than throwing — every row is therefore a
    // standalone entry, identifiable by the `standalone.webhookPath` field.
    await login(request, TEST_PASSPHRASE);
    const res = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'list' },
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as { result: Array<Record<string, unknown>> } | Array<Record<string, unknown>>;
    const rows = Array.isArray(body) ? body : body.result;
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row).toHaveProperty('standalone');
    }
  });

  test('connect with broker provider when relay is disabled returns a useful 5xx', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'connect', provider: 'github' },
    });
    expect(res.ok()).toBe(false);
    const text = await res.text();
    expect(text.toLowerCase()).toContain('relay');
  });

  test('connect with standalone openrouter returns the webhook URL', async ({ request }) => {
    await login(request, TEST_PASSPHRASE);
    const res = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'connect', provider: 'openrouter' },
    });
    expect(res.ok()).toBe(true);
    const body = await res.json() as { result: { url?: string } } | { url?: string };
    const out = (body as { result: { url?: string } }).result ?? body as { url?: string };
    expect(out.url).toContain('/hooks/openrouter-oauth');
  });

  test('connect with unknown provider returns 5xx', async ({ request }) => {
    // The fixture runs without relay enabled, so any non-standalone provider
    // hits the relay-required guard before reaching the broker's
    // unknown-provider response. Either error path is a 5xx — the precise
    // message is asserted by the dedicated 'broker provider when relay is
    // disabled' test above.
    await login(request, TEST_PASSPHRASE);
    const res = await request.post('/hooks/auth-providers', {
      headers: { 'Content-Type': 'application/json' },
      data: { action: 'connect', provider: 'no-such-provider' },
    });
    expect(res.ok()).toBe(false);
  });
});
