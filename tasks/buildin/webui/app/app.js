import { wsConnect } from './lib/ws.js';
import { navigate, route, render } from './lib/router.js';
import { setAuthOverlay } from './lib/api.js';
import { initTheme } from './lib/theme.js';

initTheme();

import './components/dc-theme-toggle.js';
import './components/dc-auth-overlay.js';
import './components/dc-task-list.js';
import './components/dc-task-detail.js';
import './components/dc-run-detail.js';
import './components/dc-config.js';
import './components/dc-secrets.js';
import './components/dc-security.js';
import './components/dc-sources.js';
import './components/dc-log-bar.js';
import './components/dc-notif-panel.js';
import './components/dc-metrics.js';
import './components/dc-relay-status.js';

// ── Auth overlay ──────────────────────────────────────────────────────────────
// Inject a single <dc-auth-overlay> into the document body. The API client
// will call overlay.show() whenever it receives a 401, showing a modal that
// lets the user authenticate without losing their current view.
const authOverlay = document.createElement('dc-auth-overlay');
document.body.appendChild(authOverlay);
setAuthOverlay(authOverlay);

// ── Router ────────────────────────────────────────────────────────────────────
const app = document.getElementById('app');

route(/^\/tasks\/(.+)$/, id => {
  const el = document.createElement('dc-task-detail');
  el.taskid = id;
  app.innerHTML = '';
  app.appendChild(el);
});

route(/^\/runs\/([^/]+)$/, id => {
  const el = document.createElement('dc-run-detail');
  el.runid = id;
  app.innerHTML = '';
  app.appendChild(el);
});

route(/^\/config$/,    () => { app.innerHTML = '<dc-config></dc-config>'; });
route(/^\/secrets$/,   () => { app.innerHTML = '<dc-secrets></dc-secrets>'; });
route(/^\/security$/,  () => { app.innerHTML = '<dc-security></dc-security>'; });
route(/^\/sources$/,   () => { app.innerHTML = '<dc-sources></dc-sources>'; });
route(/^\/metrics$/,   () => { app.innerHTML = '<dc-metrics></dc-metrics>'; });
route(/^\/$/,          () => { app.innerHTML = '<dc-task-list></dc-task-list>'; });

// Expose navigate globally for any inline hrefs
window.navigate = navigate;

// ── Relative-href SPA interceptor ─────────────────────────────────────────────
// Intercept clicks on relative hrefs (no leading /, no protocol) so they trigger
// a client-side pushState transition instead of a full page reload.
// Root-relative hrefs (/runs/…) and external links are intentionally left alone.
document.addEventListener('click', e => {
  const a = e.target.closest('a[href]');
  if (!a || a.target === '_blank') return;
  const href = a.getAttribute('href');
  if (!href || href.startsWith('/') || href.startsWith('#') || href.includes('://')) return;
  e.preventDefault();
  navigate(href === '.' ? '/' : '/' + href);
});

// ── Boot ─────────────────────────────────────────────────────────────────────
wsConnect();
render(location.pathname);
