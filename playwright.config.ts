import { defineConfig, devices } from '@playwright/test';
import path from 'path';

const BASE_URL = process.env.DICODE_URL || 'http://localhost:8080';

export default defineConfig({
  testDir: './tests/e2e',
  timeout: 30_000,
  expect: {
    timeout: 10_000,
  },
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1, // run sequentially — single shared dicode instance
  reporter: [['html', { open: 'never' }], ['list']],
  use: {
    baseURL: BASE_URL,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },

  projects: [
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
    {
      name: 'authenticated',
      testMatch: ['**/auth.spec.ts'],
      use: {
        ...devices['Desktop Chrome'],
        baseURL: BASE_URL,
      },
    },
  ],

  globalSetup: path.join(__dirname, 'tests/e2e/helpers/global-setup.ts'),
  globalTeardown: path.join(__dirname, 'tests/e2e/helpers/global-teardown.ts'),
});
