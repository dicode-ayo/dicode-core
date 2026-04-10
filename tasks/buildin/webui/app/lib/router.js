const routes = [];

// Read the base path injected by the engine (e.g. "/hooks/webui").
// Strip trailing slash so we can use it as a plain prefix.
const _base = (document.querySelector('base')?.getAttribute('href') || '/').replace(/\/$/, '');

function _stripBase(path) {
  if (_base && path.startsWith(_base)) return path.slice(_base.length) || '/';
  return path;
}

export function route(pattern, handler) {
  routes.push({ pattern, handler });
}

// navigate() accepts logical paths (e.g. "/tasks/foo") and prepends the base
// so the browser URL stays under /hooks/webui/tasks/foo.
export function navigate(path, push = true) {
  if (push) history.pushState({}, '', _base + path);
  _render(path);
}

// render() accepts either a full pathname or a logical path — strips base first.
export function render(path) {
  _render(_stripBase(path));
}

function _render(path) {
  for (const { pattern, handler } of routes) {
    const m = path.match(pattern);
    if (m) return handler(...m.slice(1));
  }
  const app = document.getElementById('app');
  if (app) app.innerHTML = '<h1>404</h1><a href="/" onclick="navigate(\'/\');return false">← Back to tasks</a>';
}

window.addEventListener('popstate', () => _render(_stripBase(location.pathname)));
