import { defineConfig, devices } from '@playwright/test';
import path from 'path';

const BASE_URL = process.env.DICODE_URL || 'http://localhost:8080';

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
    // ── unauthenticated ────────────────────────────────────────────────────────
    // Starts dicode with auth disabled. Covers the bulk of UI and API tests.
    // Run:  npx playwright test --project=unauthenticated
    {
      name: 'webui',
      testMatch: ['**/webui-task.spec.ts'],
      use: {
        ...devices['Desktop Chrome'],
        baseURL: BASE_URL,
      },
    },
    {
      name: 'unauthenticated',
      testMatch: [
        '**/task-list.spec.ts',
        '**/task-detail.spec.ts',
        '**/run-detail.spec.ts',
        '**/file-change.spec.ts',
        '**/webhooks.spec.ts',
        '**/webhooks-secure.spec.ts',
        '**/cron.spec.ts',
        '**/config.spec.ts',
      ],
      use: {
        ...devices['Desktop Chrome'],
        baseURL: BASE_URL,
      },
    },

    // ── authenticated ─────────────────────────────────────────────────────────
    // Starts dicode with auth enabled (passphrase = test-passphrase-12345).
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
