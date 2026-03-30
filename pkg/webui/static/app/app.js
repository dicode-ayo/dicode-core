import { wsConnect } from './lib/ws.js';
import { navigate, route, render } from './lib/router.js';
import { setAuthOverlay } from './lib/api.js';

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
route(/^\/$/,          () => { app.innerHTML = '<dc-task-list></dc-task-list>'; });

// Expose navigate globally for any inline hrefs
window.navigate = navigate;

// ── Boot ─────────────────────────────────────────────────────────────────────
wsConnect();
render(location.pathname);
