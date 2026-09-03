/**
 * design-tokens.spec.ts
 *
 * Contract tests for the dicode design system (theme.css + global.css from
 * dicode-buildin's webui task).
 *
 * Loads both sheets straight from disk into a blank page rather than driving
 * the real SPA: the properties under test are stylesheet-level, so there is
 * nothing to gain from booting the app, and this keeps the specs meaningful
 * even when the app's esm.sh imports are unreachable.
 *
 * Runs under the `webui` project. It needs no session — the storageState it
 * inherits there is simply unused.
 */

import { test, expect } from '@playwright/test';
import { readFileSync } from 'fs';
import path from 'path';
import { ensureBuildinCheckout } from './helpers/buildin';

const APP = path.join(ensureBuildinCheckout(path.resolve(__dirname, '../..')), 'webui/app');

const THEME = readFileSync(path.join(APP, 'theme.css'), 'utf8');
// global.css's first statement is `@import './theme.css'`, which a
// setContent() page cannot resolve — both sheets are inlined instead, so the
// import is stripped to avoid loading theme.css twice.
const GLOBAL = readFileSync(path.join(APP, 'global.css'), 'utf8').replace(
  /@import\s+['"]\.\/theme\.css['"];/,
  '',
);

const PAGE = `<!doctype html><html><head><style>${THEME}\n${GLOBAL}</style></head>
<body>
  <button class="btn" id="btn">Primary</button>
  <div class="card" id="card">card</div>
  <span class="badge badge-success" id="ok">success</span>
  <span class="badge badge-failure" id="bad">failure</span>
  <table><thead><tr><th id="th">H</th></tr></thead><tbody><tr><td id="td">c</td></tr></tbody></table>
</body></html>`;

test.describe('design tokens', () => {
  test.beforeEach(async ({ page }) => {
    await page.setContent(PAGE);
  });

  // A malformed declaration anywhere in the sheet drops rules silently rather
  // than raising, so a token-wide edit can half-break the stylesheet and still
  // look fine in a diff. Counting registered rules catches that.
  test('both sheets parse', async ({ page }) => {
    const rules = await page.evaluate(() =>
      [...document.styleSheets].reduce((n, s) => n + (s.cssRules?.length ?? 0), 0),
    );
    expect(rules).toBeGreaterThan(50);
  });

  // An undefined custom property makes its whole declaration invalid at
  // computed-value time, which shows up as an inherited or initial value
  // rather than an error — so referencing a token that does not exist fails
  // quietly. Assert every --dicode-* the sheets declare actually resolves.
  test('every --dicode-* token resolves', async ({ page }) => {
    const unresolved = await page.evaluate(() => {
      const cs = getComputedStyle(document.documentElement);
      const names = new Set<string>();
      for (const sheet of document.styleSheets) {
        for (const rule of sheet.cssRules as unknown as CSSStyleRule[]) {
          if (!rule.style) continue;
          for (const prop of rule.style) {
            if (prop.startsWith('--dicode-')) names.add(prop);
          }
        }
      }
      return [...names].filter((n) => cs.getPropertyValue(n).trim() === '');
    });
    expect(unresolved).toEqual([]);
  });

  test('token-driven declarations compute to real values', async ({ page }) => {
    const read = (id: string, prop: string) =>
      page
        .locator(`#${id}`)
        .evaluate((el, p) => getComputedStyle(el).getPropertyValue(p).trim(), prop);

    expect(await read('card', 'border-top-width')).toBe('1px'); // --dicode-border-width
    expect(await read('card', 'transition-duration')).toBe('0.15s'); // --dicode-duration-fast
    expect(await read('btn', 'border-radius')).not.toBe('0px'); // --dicode-radius-md
    expect(await read('td', 'padding-left')).not.toBe('0px'); // --dicode-space-md
    expect(await read('th', 'background-color')).not.toBe('rgba(0, 0, 0, 0)'); // --dicode-bg-alt
  });

  // The regression this exists for: badge tints were once written as rgba()
  // literals of the dark theme's hue, so flipping to light moved the text
  // color while the background kept a hue the theme no longer used. Deriving
  // them with color-mix() from the live token is what makes both move.
  test('light mode re-tints text and derived backgrounds together', async ({ page }) => {
    const sample = () =>
      page.evaluate(() => {
        const badge = document.getElementById('ok')!;
        return {
          token: getComputedStyle(document.documentElement)
            .getPropertyValue('--dicode-green')
            .trim(),
          background: getComputedStyle(badge).backgroundColor,
          color: getComputedStyle(badge).color,
        };
      });

    const dark = await sample();
    await page.evaluate(() =>
      document.documentElement.setAttribute('data-theme', 'light'),
    );
    const light = await sample();

    expect(dark.token).not.toBe(light.token);
    expect(dark.color).not.toBe(light.color);
    expect(dark.background).not.toBe(light.background);
  });

  // Distinct statuses must stay visually distinguishable, not merely carry
  // different class names.
  test('status colors stay distinct from each other', async ({ page }) => {
    const colorOf = (id: string) =>
      page.locator(`#${id}`).evaluate((el) => getComputedStyle(el).color);
    expect(await colorOf('ok')).not.toBe(await colorOf('bad'));
  });
});
