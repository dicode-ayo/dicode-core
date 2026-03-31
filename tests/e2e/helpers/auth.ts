/**
 * auth.ts
 *
 * Helpers for the authenticated test project.
 * The test passphrase is fixed in dicode-auth.yaml as server.secret.
 */

import { APIRequestContext } from '@playwright/test';

export const TEST_PASSPHRASE = 'test-passphrase-12345';
export const BASE_URL = 'http://localhost:8765';

/**
 * Login via POST /api/secrets/unlock and return the session cookie value.
 * The APIRequestContext already has a cookie jar, so after this call all
 * subsequent requests from the same context will be authenticated.
 */
export async function login(
  request: APIRequestContext,
  passphrase: string = TEST_PASSPHRASE,
): Promise<string> {
  const res = await request.post(`${BASE_URL}/api/secrets/unlock`, {
    data: { password: passphrase },
    headers: { 'Content-Type': 'application/json' },
  });
  if (!res.ok()) {
    const body = await res.text();
    throw new Error(`Login failed (${res.status()}): ${body}`);
  }
  // Return the cookie value in case the caller needs it for manual fetch calls.
  const cookies = res.headers()['set-cookie'] ?? '';
  const match = cookies.match(/dicode_secrets_sess=([^;]+)/);
  return match?.[1] ?? '';
}

/**
 * Compute an X-Hub-Signature-256 HMAC header value for a given body and secret.
 * Used in webhook-secure tests.
 */
export function hmacSignature(secret: string, body: string): string {
  const { createHmac } = require('crypto') as typeof import('crypto');
  const sig = createHmac('sha256', secret).update(body).digest('hex');
  return `sha256=${sig}`;
}
