/**
 * component-token-vars.spec.ts
 *
 * Regression coverage for #712: four var() references inside the webui
 * component sources (dc-task-list.js, dc-task-detail.js) named custom
 * properties (--badge-bg, --badge-fg, --accent, a bare --fg) that #713's
 * --dicode-* prefix migration never defined in theme.css. Three carried a
 * stale off-palette fallback (e.g. `var(--badge-bg, #2a2a2a)`), which masked
 * the bug visually while still drifting from the live theme; the fourth
 * (`var(--fg)`, no fallback) is a genuinely invalid declaration at
 * computed-value time and silently resolved to the inherited color instead.
 *
 * Booting the real SPA to exercise this would need a live daemon *and* a
 * browser that can reach https://esm.sh for Lit (dc-task-list/dc-task-detail
 * both `import ... from 'https://esm.sh/lit@3'`, and pkg/webui's CSP only
 * allows esm.sh/jsdelivr as external script sources — nothing is vendored or
 * proxied). That network dependency is unreachable from this sandbox and is
 * exactly why design-tokens.spec.ts (see its own header comment) already
 * tests this app's design-system contract by loading the stylesheets
 * straight from disk into a blank page rather than driving the real app.
 * This spec follows the same precedent, extended to the two component
 * sources: it extracts the *actual* <style> block and the *actual* inline
 * `style="..."` attribute for the affected markup straight out of the
 * shipped .js files via regex (never hand-copied), so a future edit to
 * those exact lines is still caught, then renders them against the real
 * theme.css + global.css and reads the live token values to compare
 * against — not hardcoded hex.
 *
 * Getting dc-task-detail's `files_error` panel to render via a genuine
 * pending-approval error state was also investigated and rejected: the only
 * way `pkg/approval.Gate.State`'s `inventoryOf()` call actually returns an
 * error (rather than treating a missing dir as an empty, error-free
 * inventory) is an unreadable file/dir under the task's directory, or a
 * hash_include path that escapes the sibling-task boundary — but the latter
 * is already rejected at task.yaml parse time (pkg/task/spec.go's
 * validation), before a task can ever become "pending" at all, so it never
 * reaches this code path. The permission-denied route depends on the OS
 * enforcing directory-entry permissions against the process, which root
 * (this sandbox's uid) does not — so it cannot be made to fail before the
 * fix and pass after it in an environment-independent way. Runs under the
 * `webui` project (see playwright.config.ts) — no session needed, same as
 * design-tokens.spec.ts.
 */

import { test, expect } from '@playwright/test';
import { readFileSync } from 'fs';
import path from 'path';

const APP = path.join(__dirname, '../../tasks/buildin/webui/app');
const THEME = readFileSync(path.join(APP, 'theme.css'), 'utf8');
const GLOBAL = readFileSync(path.join(APP, 'global.css'), 'utf8').replace(
  /@import\s+['"]\.\/theme\.css['"];/,
  '',
);

const TASK_LIST_SRC = readFileSync(
  path.join(APP, 'components/dc-task-list.js'),
  'utf8',
);
const TASK_DETAIL_SRC = readFileSync(
  path.join(APP, 'components/dc-task-detail.js'),
  'utf8',
);

// Pull the literal <style>...</style> block straight out of the component's
// render(), whatever var() references it currently contains — pre-fix or
// post-fix — rather than duplicating the CSS by hand.
const styleMatch = TASK_LIST_SRC.match(/<style>([\s\S]*?)<\/style>/);
if (!styleMatch) {
  throw new Error(
    'dc-task-list.js: could not find its <style> block — component markup changed shape',
  );
}
const TASK_LIST_STYLE = styleMatch[1];

// Same idea for the task-detail files_error row: capture whatever the
// current `style="..."` attribute is on the div wrapping ${st.files_error}.
const filesErrorMatch = TASK_DETAIL_SRC.match(
  /<div style="([^"]+)">\$\{st\.files_error\}<\/div>/,
);
if (!filesErrorMatch) {
  throw new Error(
    'dc-task-detail.js: could not find the files_error row — component markup changed shape',
  );
}
const FILES_ERROR_STYLE = filesErrorMatch[1];

// The wrapper's color is a marker no real token uses, so if the files_error
// div's `color` declaration is invalid at computed-value time it will
// visibly inherit this exact value instead of resolving to a token.
const MARKER_COLOR = 'rgb(1, 2, 3)';

const PAGE = `<!doctype html><html><head><style>${THEME}\n${GLOBAL}\n${TASK_LIST_STYLE}</style></head>
<body>
  <dc-task-list>
    <span class="badge-paused" id="badge-paused">paused</span>
    <button class="toggle-btn on" id="toggle-on">on</button>
  </dc-task-list>

  <div style="color:${MARKER_COLOR}">
    <div id="files-error" style="${FILES_ERROR_STYLE}">simulated files_error text</div>
  </div>

  <span id="ref-card-bg" style="background-color:var(--dicode-card-bg)"></span>
  <span id="ref-muted" style="color:var(--dicode-muted)"></span>
  <span id="ref-green" style="color:var(--dicode-green)"></span>
  <span id="ref-text" style="color:var(--dicode-text)"></span>
</body></html>`;

test.describe('component token vars (#712)', () => {
  test.beforeEach(async ({ page }) => {
    await page.setContent(PAGE);
  });

  const colorOf = (page: import('@playwright/test').Page, id: string, prop: string) =>
    page.locator(`#${id}`).evaluate((el, p) => getComputedStyle(el).getPropertyValue(p).trim(), prop);

  test('dc-task-list .badge-paused resolves to the live --dicode-card-bg / --dicode-muted tokens', async ({ page }) => {
    const badgeBg = await colorOf(page, 'badge-paused', 'background-color');
    const refBg = await colorOf(page, 'ref-card-bg', 'background-color');
    expect(badgeBg).toBe(refBg);
    expect(badgeBg).not.toBe('rgb(42, 42, 42)'); // the old var(--badge-bg, #2a2a2a) fallback

    const badgeFg = await colorOf(page, 'badge-paused', 'color');
    const refFg = await colorOf(page, 'ref-muted', 'color');
    expect(badgeFg).toBe(refFg);
    expect(badgeFg).not.toBe('rgb(170, 170, 170)'); // the old var(--badge-fg, #aaa) fallback
  });

  test('dc-task-list .toggle-btn.on resolves to the live --dicode-green token', async ({ page }) => {
    const toggleColor = await colorOf(page, 'toggle-on', 'color');
    const refGreen = await colorOf(page, 'ref-green', 'color');
    expect(toggleColor).toBe(refGreen);
    expect(toggleColor).not.toBe('rgb(76, 175, 80)'); // the old var(--accent, #4caf50) fallback
  });

  test('dc-task-detail files_error row resolves to the live --dicode-text token, not an inherited fallback', async ({ page }) => {
    const errColor = await colorOf(page, 'files-error', 'color');
    const refText = await colorOf(page, 'ref-text', 'color');
    expect(errColor).toBe(refText);
    // The old `color:var(--fg)` had no fallback, so it was an invalid
    // declaration at computed-value time and silently inherited the
    // wrapper's marker color instead of resolving to any token.
    expect(errColor).not.toBe(MARKER_COLOR);
  });
});
