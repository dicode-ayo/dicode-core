import { wsConnect } from './lib/ws.js';
import { navigate, route, render } from './lib/router.js';

import './components/dc-task-list.js';
import './components/dc-task-detail.js';
import './components/dc-run-detail.js';
import './components/dc-config.js';
import './components/dc-secrets.js';
import './components/dc-log-bar.js';
import './components/dc-notif-panel.js';

// ── Router ────────────────────────────────────────────────────────────────────
const app = document.getElementById('app');

route(/^\/tasks\/([^/]+)$/, id => {
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

route(/^\/config$/,  () => { app.innerHTML = '<dc-config></dc-config>'; });
route(/^\/secrets$/, () => { app.innerHTML = '<dc-secrets></dc-secrets>'; });
route(/^\/$/,        () => { app.innerHTML = '<dc-task-list></dc-task-list>'; });

// Expose navigate globally for any inline hrefs
window.navigate = navigate;

// ── Boot ─────────────────────────────────────────────────────────────────────
wsConnect();
render(location.pathname);
