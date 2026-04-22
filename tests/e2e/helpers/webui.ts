import type { Page } from '@playwright/test';

export const WEBUI_URL = '/hooks/webui';

declare global {
  interface Window {
    navigate: (path: string) => void;
  }
}

/** Navigate to the webui root and wait until the SPA is ready. */
export async function gotoWebui(page: Page): Promise<void> {
  await page.goto(WEBUI_URL);
  await waitForTaskList(page);
  // Lit components hydrate from esm.sh asynchronously and cause layout shifts
  // while loading. Wait for network idle so click actionability checks don't
  // fail with "element is not stable".
  await page.waitForLoadState('networkidle', { timeout: 15_000 }).catch(() => undefined);
}

/** Wait for the task list component to finish loading. */
export async function waitForTaskList(page: Page): Promise<void> {
  await page.waitForSelector('dc-task-list', { timeout: 10_000 });
  await page.waitForFunction(() => {
    const el = document.querySelector('dc-task-list');
    return !!el && !el.textContent?.includes('Loading');
  }, { timeout: 15_000 });
}

/** Wait for the task detail component to finish loading. */
export async function waitForTaskDetail(page: Page): Promise<void> {
  await page.waitForSelector('dc-task-detail', { timeout: 10_000 });
  await page.waitForFunction(() => {
    const el = document.querySelector('dc-task-detail');
    return !!el && !el.textContent?.includes('Loading');
  }, { timeout: 15_000 });
}

/** Wait for the run detail component to finish loading. */
export async function waitForRunDetail(page: Page): Promise<void> {
  await page.waitForSelector('dc-run-detail', { timeout: 10_000 });
  await page.waitForFunction(() => {
    const el = document.querySelector('dc-run-detail');
    return !!el && !el.textContent?.includes('Loading');
  }, { timeout: 15_000 });
}

/** Navigate the SPA client-side via window.navigate. Caller should already be on /hooks/webui. */
export async function navigateInSpa(page: Page, spaPath: string): Promise<void> {
  await page.evaluate((p) => window.navigate(p), spaPath);
}
