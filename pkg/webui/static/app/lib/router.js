const routes = [];

export function route(pattern, handler) {
  routes.push({ pattern, handler });
}

export function navigate(path, push = true) {
  if (push) history.pushState({}, '', path);
  _render(path);
}

export function render(path) {
  _render(path);
}

function _render(path) {
  for (const { pattern, handler } of routes) {
    const m = path.match(pattern);
    if (m) return handler(...m.slice(1));
  }
  const app = document.getElementById('app');
  if (app) app.innerHTML = '<h1>404</h1><a href="/" onclick="navigate(\'/\');return false">← Back to tasks</a>';
}

window.addEventListener('popstate', () => _render(location.pathname));
