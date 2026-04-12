// Shared stylesheet — injected into index.html <style> and adopted by
// Shadow DOM components via adoptedStyleSheets.
//
// All colors/spacing/typography reference CSS variables defined in theme.css.
// This keeps the webui in sync with dicode-site and dicode docs.
//
// Note: since Shadow DOM doesn't inherit :root variables by default, any
// Shadow DOM component that adopts this sheet also needs to forward
// :host from the light DOM (or use light DOM via createRenderRoot).
export const SHARED_CSS = `
  * { box-sizing: border-box; margin: 0; padding: 0; }

  body {
    font-family: var(--font-sans, system-ui, sans-serif);
    background: var(--bg, #f7f8fc);
    color: var(--text, #1a1a2e);
    line-height: var(--leading-normal, 1.6);
  }

  header {
    background: var(--bg-alt, #eef1f8);
    color: var(--heading, #0d0d1a);
    padding: var(--space-sm, .5rem) var(--space-lg, 1.5rem);
    display: flex;
    align-items: center;
    gap: var(--space-md, 1rem);
    border-bottom: 1px solid var(--border, rgba(13,13,26,.1));
  }
  header a { color: var(--sky, #0d6efd); text-decoration: none; font-weight: var(--font-semibold, 600); }
  nav a {
    color: var(--muted, #5c6680);
    text-decoration: none;
    margin-left: var(--space-md, 1rem);
    font-size: var(--text-base, .9rem);
  }
  nav a:hover { color: var(--sky, #0d6efd); }

  main { padding: var(--space-lg, 1.5rem); max-width: 1100px; margin: 0 auto; }

  h1 {
    font-size: var(--text-xl, 1.4rem);
    margin-bottom: var(--space-md, 1rem);
    color: var(--heading, #0d0d1a);
    font-weight: var(--font-bold, 700);
  }
  h2 {
    font-size: var(--text-lg, 1.15rem);
    margin-bottom: var(--space-sm, .5rem);
    margin-top: var(--space-md, 1rem);
    color: var(--heading, #0d0d1a);
    font-weight: var(--font-bold, 700);
  }

  /* Tables */
  table {
    width: 100%;
    border-collapse: collapse;
    background: var(--card-bg, #fff);
    border: 1px solid var(--border, rgba(13,13,26,.1));
    border-radius: var(--radius-sm, 6px);
    overflow: hidden;
  }
  th, td {
    padding: var(--space-sm, .6rem) var(--space-md, 1rem);
    text-align: left;
    border-bottom: 1px solid var(--border, rgba(13,13,26,.1));
    font-size: var(--text-base, .9rem);
  }
  th {
    background: var(--bg-alt, #eef1f8);
    font-weight: var(--font-semibold, 600);
    color: var(--heading, #0d0d1a);
  }
  td { color: var(--text, #1a1a2e); }
  tr:last-child td { border-bottom: none; }

  a { color: var(--blue, #0d6efd); }

  /* Badges */
  .badge {
    display: inline-block;
    padding: 0.2em 0.6em;
    border-radius: var(--radius-sm, 6px);
    font-size: var(--text-xs, .72rem);
    font-weight: var(--font-semibold, 600);
  }
  .badge-success  { background: rgba(22, 163, 74, .15);  color: var(--green, #16a34a);  border: 1px solid rgba(22, 163, 74, .3); }
  .badge-failure  { background: rgba(220, 38, 38, .15);  color: var(--red, #dc2626);    border: 1px solid rgba(220, 38, 38, .3); }
  .badge-running  { background: rgba(202, 138, 4, .15);  color: var(--yellow, #ca8a04); border: 1px solid rgba(202, 138, 4, .3); }
  .badge-cancelled{ background: var(--card-bg);          color: var(--muted, #5c6680);  border: 1px solid var(--border); }
  .badge-manual   { background: var(--card-bg);          color: var(--muted, #5c6680);  border: 1px solid var(--border); }

  /* Buttons */
  .btn {
    display: inline-block;
    padding: var(--space-sm, .4rem) var(--space-md, .9rem);
    border-radius: var(--radius-md, 10px);
    border: none;
    cursor: pointer;
    font-size: var(--text-base, .85rem);
    font-weight: var(--font-semibold, 600);
    background: var(--blue, #0d6efd);
    color: #fff;
    text-decoration: none;
    transition: background var(--duration-fast, 150ms) var(--ease, ease);
  }
  .btn:hover { background: var(--blue2, #3a8bfd); }
  .btn.secondary {
    background: var(--card-bg);
    color: var(--text);
    border: 1px solid var(--border);
  }
  .btn.secondary:hover { background: var(--blue-tint); border-color: var(--blue); color: var(--blue); }
  .btn-sm { padding: var(--space-xs, .25rem) var(--space-sm, .6rem); font-size: var(--text-xs, .78rem); }

  :focus-visible { outline: 2px solid var(--blue, #0d6efd); outline-offset: 2px; border-radius: 2px; }
  .btn:focus-visible { outline: 2px solid #fff; outline-offset: 2px; }

  /* Code blocks */
  pre {
    background: var(--code-bg, #1e1e2e);
    color: var(--code-text, #e8eaf0);
    padding: var(--space-md, 1rem);
    border: 1px solid var(--code-border, rgba(160,196,255,.15));
    border-radius: var(--radius-sm, 6px);
    overflow-x: auto;
    font-size: var(--text-sm, .82rem);
    font-family: var(--font-mono, monospace);
    line-height: var(--leading-normal, 1.5);
    white-space: pre-wrap;
  }

  /* Cards */
  .card {
    background: var(--card-bg);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm, 6px);
    padding: var(--space-md) var(--space-lg);
    margin-bottom: var(--space-md);
  }

  .meta { font-size: var(--text-sm, .82rem); color: var(--muted, #5c6680); }

  /* Form inputs */
  .input {
    padding: var(--space-sm, .4rem);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm, 4px);
    background: var(--card-bg);
    color: var(--text);
    font-family: inherit;
  }
  .input:focus { outline: none; border-color: var(--blue); }

  .cfg-form input, .cfg-form select {
    background: var(--card-bg);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm, 4px);
    padding: var(--space-xs, .35rem) var(--space-sm, .5rem);
    font-size: var(--text-base, .85rem);
    width: 100%;
    box-sizing: border-box;
    font-family: inherit;
  }
  .cfg-form input:focus, .cfg-form select:focus { outline: none; border-color: var(--blue); }
  .cfg-form label {
    font-size: var(--text-xs, .78rem);
    color: var(--muted);
    display: block;
    margin-bottom: var(--space-xs, .25rem);
  }
  .cfg-form .field { margin-bottom: var(--space-sm, .75rem); }
  .cfg-form .hint { font-size: var(--text-xs, .72rem); color: var(--muted); margin-top: var(--space-xs, .2rem); }
`;

// For Shadow DOM adoption (unused if all components use light DOM)
let _sheet = null;
export function getSharedSheet() {
  if (!_sheet) {
    _sheet = new CSSStyleSheet();
    _sheet.replaceSync(SHARED_CSS);
  }
  return _sheet;
}
