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
 * Login via POST /api/auth/login and return the session cookie value.
 * The APIRequestContext already has a cookie jar, so after this call all
 * subsequent requests from the same context will be authenticated.
 */
export async function login(
  request: APIRequestContext,
  passphrase: string = TEST_PASSPHRASE,
): Promise<string> {
  const res = await request.post(`${BASE_URL}/api/auth/login`, {
    data: { password: passphrase },
    headers: { 'Content-Type': 'application/json' },
  });
  if (!res.ok()) {
    const body = await res.text();
    throw new Error(`Login failed (${res.status()}): ${body}`);
  }
  const cookies = res.headers()['set-cookie'] ?? '';
  const match = cookies.match(/dicode_secrets_sess=([^;]+)/);
  return match?.[1] ?? '';
}
