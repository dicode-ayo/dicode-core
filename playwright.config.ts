import { defineConfig, devices } from '@playwright/test';
import path from 'path';

const BASE_URL = process.env.DICODE_URL || 'http://localhost:8765';

// Written by global-setup.ts (helpers/dicode-server.ts -> writeAuthState).
// Must match AUTH_STATE_PATH in that file. Fixed path lets Playwright's
// per-project `use.storageState` resolve at config-load time — globalSetup
// runs AFTER config eval, so an env-var-based path wouldn't work.
const AUTH_STATE = path.join(__dirname, 'tests/e2e/.auth-state.json');

export default defineConfig({
  testDir: './tests/e2e',
  timeout: 30_000,
  expect: {
    timeout: 10_000,
  },
  // Run tests serially — we share a single dicode process.
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: [['html', { open: 'never' }], ['list']],
  use: {
    baseURL: BASE_URL,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },

  projects: [
    // ── webui ─────────────────────────────────────────────────────────────────
    // Browser tests for the SPA at /hooks/webui (which has trigger.auth: true).
    // Uses the seeded session from global-setup.
    {
      name: 'webui',
      testMatch: ['**/webui-task.spec.ts'],
      use: {
        ...devices['Desktop Chrome'],
        baseURL: BASE_URL,
        storageState: AUTH_STATE,
      },
    },

    // ── unauthenticated ────────────────────────────────────────────────────────
    // server.auth=false, no passphrase. API tests do not need a session, but
    // UI tests do (webui task is auth-gated). Loading storageState is harmless
    // for API-only specs and required for UI ones.
    {
      name: 'unauthenticated',
      testMatch: [
        '**/file-change.spec.ts',
        '**/webhooks.spec.ts',
        '**/webhooks-secure.spec.ts',
        '**/cron.spec.ts',
        '**/config.spec.ts',
      ],
      use: {
        ...devices['Desktop Chrome'],
        baseURL: BASE_URL,
        storageState: AUTH_STATE,
      },
    },

    // ── authenticated ─────────────────────────────────────────────────────────
    // server.auth=true, passphrase = test-passphrase-12345.
    // No storageState — auth.spec.ts tests the login flow itself.
    // Run:  DICODE_AUTH_MODE=authenticated npx playwright test --project=authenticated
    {
      name: 'authenticated',
      testMatch: ['**/auth.spec.ts'],
      use: {
        ...devices['Desktop Chrome'],
        baseURL: BASE_URL,
      },
    },
  ],

  // Global setup builds the binary, writes temp configs, and starts dicode.
  // Set DICODE_AUTH_MODE=authenticated before running to use the auth config.
  globalSetup: path.join(__dirname, 'tests/e2e/helpers/global-setup.ts'),
  globalTeardown: path.join(__dirname, 'tests/e2e/helpers/global-teardown.ts'),
});
