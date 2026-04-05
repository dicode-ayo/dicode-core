// Shared stylesheet — injected into index.html <style> and adopted by
// Shadow DOM components via adoptedStyleSheets.
export const SHARED_CSS = `
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: system-ui, sans-serif; background: #f8f9fa; color: #212529; }
  header { background: #1a1a2e; color: #fff; padding: 0.75rem 1.5rem; display: flex; align-items: center; gap: 1rem; }
  header a { color: #a0c4ff; text-decoration: none; font-weight: 600; }
  nav a { color: #ccc; text-decoration: none; margin-left: 1rem; font-size: 0.9rem; }
  main { padding: 1.5rem; max-width: 1100px; margin: 0 auto; }
  h1 { font-size: 1.4rem; margin-bottom: 1rem; }
  h2 { font-size: 1.1rem; margin-bottom: 0.5rem; margin-top: 1rem; }
  table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 6px; overflow: hidden; box-shadow: 0 1px 3px rgba(0,0,0,.08); }
  th, td { padding: 0.6rem 1rem; text-align: left; border-bottom: 1px solid #eee; font-size: 0.9rem; }
  th { background: #f1f3f5; font-weight: 600; }
  tr:last-child td { border-bottom: none; }
  a { color: #0d6efd; }
  .badge { display: inline-block; padding: 0.2em 0.6em; border-radius: 4px; font-size: 0.78rem; font-weight: 600; }
  .badge-success  { background: #d1e7dd; color: #0f5132; }
  .badge-failure  { background: #f8d7da; color: #842029; }
  .badge-running  { background: #fff3cd; color: #664d03; }
  .badge-cancelled{ background: #e2e3e5; color: #383d41; }
  .badge-manual   { background: #e2e3e5; color: #383d41; }
  .btn { display: inline-block; padding: 0.4rem 0.9rem; border-radius: 5px; border: none; cursor: pointer; font-size: 0.85rem; font-weight: 600; background: #0d6efd; color: #fff; text-decoration: none; }
  .btn:hover { background: #0b5ed7; }
  .btn.secondary { background: #6c757d; }
  .btn.secondary:hover { background: #5a6268; }
  .btn-sm { padding: 0.25rem 0.6rem; font-size: 0.78rem; }
  :focus-visible { outline: 2px solid #0d6efd; outline-offset: 2px; border-radius: 2px; }
  .btn:focus-visible { outline: 2px solid #fff; outline-offset: 2px; }
  pre { background: #1e1e2e; color: #cdd6f4; padding: 1rem; border-radius: 6px; overflow-x: auto; font-size: 0.82rem; line-height: 1.5; white-space: pre-wrap; }
  .card { background: #fff; border-radius: 6px; box-shadow: 0 1px 3px rgba(0,0,0,.08); padding: 1rem 1.25rem; margin-bottom: 1rem; }
  .meta { font-size: 0.82rem; color: #6c757d; }
  .input { padding: 0.4rem; border: 1px solid #ccc; border-radius: 4px; }
  .cfg-form input, .cfg-form select {
    background: #2a2a3e; color: #cdd6f4; border: 1px solid #444; border-radius: 4px;
    padding: 0.35rem 0.5rem; font-size: 0.85rem; width: 100%; box-sizing: border-box;
  }
  .cfg-form label { font-size: 0.78rem; color: #888; display: block; margin-bottom: 0.25rem; }
  .cfg-form .field { margin-bottom: 0.75rem; }
  .cfg-form .hint { font-size: 0.72rem; color: #666; margin-top: 0.2rem; }
  .task-desc { font-size: 0.875rem; color: #444; line-height: 1.65; margin-bottom: 0.75rem; }
  .task-desc p  { margin-bottom: 0.5rem; }
  .task-desc p:last-child { margin-bottom: 0; }
  .task-desc h2 { font-size: 0.875rem; font-weight: 700; color: #212529; margin: 0.85rem 0 0.3rem; }
  .task-desc h3 { font-size: 0.825rem; font-weight: 600; color: #495057; margin: 0.6rem 0 0.2rem; }
  .task-desc ul, .task-desc ol { padding-left: 1.4rem; margin-bottom: 0.5rem; }
  .task-desc li { margin-bottom: 0.2rem; }
  .task-desc li p { margin-bottom: 0; }
  .task-desc ol > li { padding-left: 0.15rem; margin-bottom: 0.45rem; }
  .task-desc ol > li::marker { font-weight: 700; color: #495057; }
  .task-desc ol > li > ul { margin-top: 0.3rem; margin-bottom: 0; padding-left: 1.1rem; }
  .task-desc ol > li > ul > li { margin-bottom: 0.15rem; list-style-type: disc; }
  .task-desc code { background: #f1f3f5; color: #c92a2a; padding: 0.1em 0.35em; border-radius: 3px; font-size: 0.82em; font-family: ui-monospace,SFMono-Regular,Menlo,monospace; }
  .task-desc pre { margin: 0.5rem 0; }
  .task-desc pre code { background: none; color: inherit; padding: 0; }
  .task-desc a { color: #0d6efd; }
  .task-desc hr { border: none; border-top: 1px solid #dee2e6; margin: 0.75rem 0; }
  .task-desc strong { color: #212529; }
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
