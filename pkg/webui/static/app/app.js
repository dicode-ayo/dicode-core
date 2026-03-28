// ── Config ──────────────────────────────────────────────────────────────────
const API = '';  // same origin

// ── REST helpers ────────────────────────────────────────────────────────────
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(API + path, opts);
  if (!res.ok) throw new Error(await res.text());
  const ct = res.headers.get('Content-Type') || '';
  if (ct.includes('application/json')) return res.json();
  return res.text();
}
const get  = (path) => api('GET', path);
const post = (path, body) => api('POST', path, body);
const del  = (path) => api('DELETE', path);

// ── WebSocket ────────────────────────────────────────────────────────────────
let ws = null;
const wsHandlers = {};  // type → [fn]

function wsConnect() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/ws`);
  ws.onopen = () => {
    wsSend('sub:logs');
    setLogStatus('● connected', '#a6e3a1');
  };
  ws.onmessage = (e) => {
    try {
      const msg = JSON.parse(e.data);
      (wsHandlers[msg.type] || []).forEach(fn => fn(msg.data));
    } catch(_) {}
  };
  ws.onclose = () => {
    ws = null;
    setLogStatus('● disconnected', '#f38ba8');
    setTimeout(wsConnect, 3000);
  };
  ws.onerror = () => { if (ws) ws.close(); };
}
function setLogStatus(text, color) {
  const el = document.getElementById('logstatus');
  if (el) { el.textContent = text; el.style.color = color; }
}
function wsSend(type, data) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type, data }));
  }
}
function wsOn(type, fn) {
  if (!wsHandlers[type]) wsHandlers[type] = [];
  wsHandlers[type].push(fn);
  return () => { wsHandlers[type] = wsHandlers[type].filter(f => f !== fn); };
}

// ── Router ───────────────────────────────────────────────────────────────────
const routes = [];
function route(pattern, handler) { routes.push({ pattern, handler }); }

function navigate(path, push = true) {
  if (push) history.pushState({}, '', path);
  render(path);
}
window.addEventListener('popstate', () => render(location.pathname));

function render(path) {
  for (const { pattern, handler } of routes) {
    const m = path.match(pattern);
    if (m) return handler(...m.slice(1));
  }
  renderNotFound();
}

// ── Notifications ────────────────────────────────────────────────────────────
const STORAGE_KEY = 'dicode_notifs';
let unread = 0, inboxOpen = false;

function loadNotifs() { try { return JSON.parse(localStorage.getItem(STORAGE_KEY)||'[]'); } catch(_) { return []; } }
function saveNotifs(list) { localStorage.setItem(STORAGE_KEY, JSON.stringify(list.slice(-50))); }

function addNotif(evt) {
  const list = loadNotifs();
  list.push({ ts: Date.now(), runID: evt.runID, taskName: evt.taskName, taskID: evt.taskID, status: evt.status, durationMs: evt.durationMs });
  saveNotifs(list);
  renderInbox();
  if (!inboxOpen) { unread++; updateBadge(); }
  if (swReg && swReg.active && Notification.permission === 'granted') {
    swReg.active.postMessage({ type: 'run:complete', ...evt });
  }
}

function renderInbox() {
  const el = document.getElementById('notif-list');
  if (!el) return;
  const list = loadNotifs().slice().reverse();
  if (!list.length) { el.innerHTML = '<div style="padding:1rem;color:#888;text-align:center">No notifications yet.</div>'; return; }
  el.innerHTML = list.map(n => {
    const ago = Math.round((Date.now()-n.ts)/60000);
    const agoStr = ago < 1 ? 'just now' : ago < 60 ? ago+'m ago' : Math.round(ago/60)+'h ago';
    const ok = n.status === 'success';
    const icon = ok ? '✓' : '✗', color = ok ? '#0f5132' : '#842029', bg = ok ? '#d1e7dd' : '#f8d7da';
    return `<div style="display:flex;align-items:center;gap:0.5rem;padding:0.6rem 1rem;border-bottom:1px solid #f0f0f0">
      <span style="background:${bg};color:${color};border-radius:4px;padding:0.1em 0.5em;font-weight:600;font-size:0.78rem">${icon} ${n.status}</span>
      <div style="flex:1;min-width:0"><div style="font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(n.taskName)}</div>
      <div style="color:#888;font-size:0.75rem">${agoStr}</div></div>
      <a href="/runs/${n.runID}" onclick="navigate('/runs/${n.runID}');return false" style="font-size:0.75rem;white-space:nowrap">View →</a>
    </div>`;
  }).join('');
}

function updateBadge() {
  const b = document.getElementById('notif-badge');
  if (!b) return;
  if (unread > 0) { b.textContent = unread > 99 ? '99+' : unread; b.style.display = 'block'; }
  else b.style.display = 'none';
}

// ── Log bar ───────────────────────────────────────────────────────────────────
let logBarOpen = false, logCount = 0;
const levelColor = { DEBUG:'#89b4fa', INFO:'#a6e3a1', WARN:'#f9e2af', ERROR:'#f38ba8' };

function logBarAppend(line) {
  const el = document.getElementById('logconsole');
  if (!el) return;
  let text = line, color = '';
  try {
    const j = JSON.parse(line);
    const lvl = (j.level||'info').toUpperCase();
    const ts = j.ts ? new Date(j.ts*1000).toLocaleTimeString() : '';
    const extra = Object.entries(j).filter(([k])=>!['level','ts','msg','caller','stacktrace'].includes(k)).map(([k,v])=>k+'='+JSON.stringify(v)).join(' ');
    text = ts+' '+lvl.padEnd(5)+' '+(j.msg||'')+(extra?' '+extra:'');
    color = levelColor[lvl]||'';
  } catch(_) {}
  logCount++;
  const cnt = document.getElementById('logcount');
  if (cnt) cnt.textContent = logCount+' lines';
  const span = document.createElement('span');
  if (color) span.style.color = color;
  span.textContent = text+'\n';
  el.appendChild(span);
  if (logBarOpen) el.scrollTop = el.scrollHeight;
}

function toggleLogBar() {
  logBarOpen = !logBarOpen;
  const el = document.getElementById('logconsole');
  const arrow = document.getElementById('logarrow');
  if (el) el.style.display = logBarOpen ? 'block' : 'none';
  if (arrow) arrow.textContent = logBarOpen ? '▼' : '▶';
  if (logBarOpen && el) el.scrollTop = el.scrollHeight;
}

// ── Helper: mount HTML into #app ─────────────────────────────────────────────
function mount(html) {
  const app = document.getElementById('app');
  app.innerHTML = html;
  return app;
}

// ── Task List page ──────────────────────────────────────────────────────────
async function renderTaskList() {
  mount('<h1>Tasks</h1><div class="meta">Loading…</div>');
  let tasks;
  try { tasks = await get('/api/tasks'); } catch(e) { mount(`<p style="color:red">Error: ${esc(e.message)}</p>`); return; }

  const rows = (tasks||[]).map(t => `
    <tr data-task-id="${t.id}">
      <td><a href="/tasks/${t.id}" onclick="navigate('/tasks/${t.id}');return false">${esc(t.id)}</a></td>
      <td>${esc(t.name)}</td>
      <td><span class="meta">${esc(t.trigger_label||'manual')}</span></td>
      <td id="task-last-run-${t.id}">${t.last_run_id ? `<a href="/runs/${t.last_run_id}" onclick="navigate('/runs/${t.last_run_id}');return false">${t.last_run_id.slice(0,8)}</a>` : '—'}</td>
      <td id="task-last-status-${t.id}">${t.last_run_status ? `<span class="badge badge-${t.last_run_status}">${t.last_run_status}</span>` : '—'}</td>
      <td><button class="btn btn-sm" onclick="runTask('${t.id}')">&#9654; Run</button></td>
    </tr>`).join('');

  mount(`
    <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem">
      <h1 style="margin:0">Tasks</h1>
    </div>
    <table>
      <thead><tr><th>ID</th><th>Name</th><th>Trigger</th><th>Last Run</th><th>Status</th><th></th></tr></thead>
      <tbody>${rows||'<tr><td colspan="6" style="text-align:center;color:#888;padding:2rem">No tasks found. Add tasks to your data directory.</td></tr>'}</tbody>
    </table>`);
}

async function runTask(taskID) {
  try {
    const r = await post(`/api/tasks/${taskID}/run`);
    navigate(`/runs/${r.runId}`);
  } catch(e) { alert('Failed to run task: '+e.message); }
}

// ── Task Detail page ────────────────────────────────────────────────────────
let editorInstance = null;
let currentTaskID = null;
let currentEditorFile = null;

async function renderTaskDetail(taskID) {
  currentTaskID = taskID;
  mount('<div class="meta">Loading…</div>');
  let task, runs;
  try {
    [task, runs] = await Promise.all([get(`/api/tasks/${taskID}`), get(`/api/tasks/${taskID}/runs?limit=20`)]);
  } catch(e) { mount(`<p style="color:red">Error: ${esc(e.message)}</p>`); return; }

  const runRows = (runs||[]).map(r => `
    <tr data-run-id="${r.ID}">
      <td><a href="/runs/${r.ID}" onclick="navigate('/runs/${r.ID}');return false">${r.ID.slice(0,8)}</a></td>
      <td><span class="badge badge-${r.Status}">${r.Status}</span></td>
      <td class="meta">${fmtTime(r.StartedAt)}</td>
      <td class="meta">${fmtDuration(r.StartedAt, r.FinishedAt)}</td>
    </tr>`).join('');

  mount(`
    <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.5rem">
      <h1 style="margin:0">${esc(task.name)}</h1>
      <button class="btn" onclick="runTask('${taskID}')">&#9654; Run now</button>
      <button class="btn" style="background:#495057" onclick="openEditor('${taskID}')">&#9998; Edit code</button>
    </div>
    ${task.description ? `<p class="meta" style="margin-bottom:0.75rem">${esc(task.description)}</p>` : ''}

    <div class="card" style="margin-bottom:1rem;display:flex;align-items:center;gap:0.75rem">
      <span style="font-size:0.85rem"><strong>Trigger:</strong> ${esc(task.trigger_label||'manual')}</span>
      <button class="btn btn-sm" style="background:#495057;margin-left:auto" onclick="openTriggerEditor('${taskID}', ${JSON.stringify(JSON.stringify(task.Trigger||{}))})">&#9998; Edit trigger</button>
    </div>

    <div id="trigger-editor-mount"></div>
    <div id="editor-mount"></div>

    <h2>Recent runs</h2>
    <table>
      <thead><tr><th>Run ID</th><th>Status</th><th>Started</th><th>Duration</th></tr></thead>
      <tbody id="runs-tbody">${runRows||'<tr><td colspan="4" style="text-align:center;color:#888">No runs yet.</td></tr>'}</tbody>
    </table>`);

  // Live updates: when a run starts/finishes for this task, update the table
  const offStarted = wsOn('run:started', d => {
    if (d.taskID !== taskID) return;
    const tbody = document.getElementById('runs-tbody');
    if (!tbody) { offStarted(); return; }
    const row = document.createElement('tr');
    row.dataset.runId = d.runID;
    row.innerHTML = `
      <td><a href="/runs/${d.runID}" onclick="navigate('/runs/${d.runID}');return false">${d.runID.slice(0,8)}</a></td>
      <td><span class="badge badge-running" id="run-status-${d.runID}">running</span></td>
      <td class="meta">${new Date().toLocaleTimeString()}</td>
      <td class="meta">—</td>`;
    tbody.prepend(row);
  });

  const offFinished = wsOn('run:finished', d => {
    if (d.taskID !== taskID) return;
    const badge = document.getElementById(`run-status-${d.runID}`);
    if (!badge) { offFinished(); return; }
    badge.className = `badge badge-${d.status}`;
    badge.textContent = d.status;
  });
}

// ── Code Editor ─────────────────────────────────────────────────────────────
async function openEditor(taskID) {
  const task = await get(`/api/tasks/${taskID}`);
  const edMount = document.getElementById('editor-mount');
  if (!edMount) return;

  const scriptFile = task.script_file || 'task.ts';
  const testFile = task.test_file || scriptFile.replace(/\.(ts|js)$/, '.test.$1');

  edMount.innerHTML = `
    <div class="card" id="editor-panel" style="margin-top:1.5rem;padding:0.75rem">
      <div style="display:flex;align-items:center;gap:0.5rem;margin-bottom:0.5rem;flex-wrap:wrap">
        <button id="tab-script" class="btn btn-sm" onclick="editorSwitchTab('${scriptFile}')">${scriptFile}</button>
        ${task.test_exists ? `<button id="tab-test" class="btn btn-sm secondary" onclick="editorSwitchTab('${testFile}')">${testFile}</button>` : ''}
        <div style="margin-left:auto;display:flex;gap:0.5rem;align-items:center">
          <span id="editor-status" style="font-size:0.8rem"></span>
          <button class="btn btn-sm" onclick="editorSave()">&#128190; Save</button>
          <button id="ai-toggle-btn" class="btn btn-sm" style="background:#7c3aed" onclick="aiToggle()">&#129302; AI</button>
          <button class="btn btn-sm secondary" onclick="editorClose()">✕ Close</button>
        </div>
      </div>
      <div style="display:flex;gap:0.75rem;align-items:stretch">
        <div id="monaco-container" style="flex:1;min-width:0;height:440px;border-radius:4px;overflow:hidden"></div>
        <div id="ai-panel" style="display:none;width:360px;flex-shrink:0;flex-direction:column;background:#13131f;border-radius:6px;border:1px solid #2a2a4a;overflow:hidden">
          <div style="padding:0.5rem 0.75rem;background:#1a1a2e;border-bottom:1px solid #2a2a4a;display:flex;align-items:center;gap:0.5rem">
            <span style="color:#a0c4ff;font-weight:600;font-size:0.85rem">&#129302; AI Task Dev</span>
          </div>
          <div id="ai-history" style="flex:1;overflow-y:auto;padding:0.75rem;font-size:0.8rem;font-family:system-ui,sans-serif;color:#cdd6f4;min-height:240px;max-height:300px;line-height:1.5"></div>
          <div id="ai-status" style="padding:0.2rem 0.75rem;font-size:0.7rem;color:#7c7ca8;border-top:1px solid #2a2a4a;min-height:1.4rem;font-family:monospace"></div>
          <div style="padding:0.5rem;border-top:1px solid #2a2a4a;display:flex;flex-direction:column;gap:0.4rem">
            <textarea id="ai-input" placeholder="Describe the task you want… (Ctrl+Enter to send)" style="width:100%;background:#0e0e1a;color:#cdd6f4;border:1px solid #2a2a4a;border-radius:4px;padding:0.45rem 0.5rem;font-size:0.78rem;resize:none;height:72px;font-family:system-ui,sans-serif;outline:none;box-sizing:border-box" onkeydown="if((event.ctrlKey||event.metaKey)&&event.key==='Enter'){aiSend();event.preventDefault()}"></textarea>
            <div style="display:flex;gap:0.4rem;align-items:center">
              <button class="btn btn-sm" onclick="aiSend()" style="background:#7c3aed;flex:1">Send</button>
            </div>
          </div>
        </div>
      </div>
    </div>`;

  currentEditorFile = scriptFile;
  await loadEditorFile(taskID, scriptFile);
}

async function loadEditorFile(taskID, filename) {
  currentEditorFile = filename;
  try {
    const content = await fetch(`/api/tasks/${taskID}/files/${filename}`).then(r => r.ok ? r.text() : Promise.reject(new Error('not found')));
    const lang = filename.endsWith('.ts') ? 'typescript' : filename.endsWith('.py') ? 'python' : 'javascript';
    if (!editorInstance) {
      require.config({ paths: { vs: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs' } });
      require(['vs/editor/editor.main'], () => {
        editorInstance = monaco.editor.create(document.getElementById('monaco-container'), {
          value: content,
          language: lang,
          theme: 'vs-dark',
          fontSize: 13,
          minimap: { enabled: false },
          scrollBeyondLastLine: false,
        });
      });
    } else {
      const model = editorInstance.getModel();
      if (model) {
        monaco.editor.setModelLanguage(model, lang);
        editorInstance.setValue(content);
      }
    }
  } catch(e) {
    const st = document.getElementById('editor-status');
    if (st) st.textContent = 'Error: '+e.message;
  }
}

function editorSwitchTab(filename) {
  if (currentTaskID) loadEditorFile(currentTaskID, filename);
}

async function editorSave() {
  if (!editorInstance || !currentTaskID || !currentEditorFile) return;
  const st = document.getElementById('editor-status');
  if (st) st.textContent = 'Saving…';
  try {
    await fetch(`/api/tasks/${currentTaskID}/files/${currentEditorFile}`, {
      method: 'POST',
      headers: { 'Content-Type': 'text/plain' },
      body: editorInstance.getValue(),
    });
    if (st) { st.textContent = 'Saved ✓'; setTimeout(() => { if(st) st.textContent=''; }, 2000); }
  } catch(e) {
    if (st) st.textContent = 'Error: '+e.message;
  }
}

function editorClose() {
  if (editorInstance) { editorInstance.dispose(); editorInstance = null; }
  const panel = document.getElementById('editor-panel');
  if (panel) panel.remove();
}

function aiToggle() {
  const panel = document.getElementById('ai-panel');
  if (!panel) return;
  const visible = panel.style.display === 'flex';
  panel.style.display = visible ? 'none' : 'flex';
}

async function aiSend() {
  const input = document.getElementById('ai-input');
  const history = document.getElementById('ai-history');
  const status = document.getElementById('ai-status');
  if (!input || !history || !currentTaskID) return;
  const msg = input.value.trim();
  if (!msg) return;
  input.value = '';
  history.innerHTML += `<div style="margin-bottom:0.5rem"><strong style="color:#a0c4ff">You:</strong> ${esc(msg)}</div>`;
  if (status) status.textContent = 'Thinking…';

  const response = await fetch(`/api/tasks/${currentTaskID}/ai/stream`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ message: msg }),
  });

  const aiDiv = document.createElement('div');
  aiDiv.style.marginBottom = '0.5rem';
  aiDiv.innerHTML = '<strong style="color:#a6e3a1">AI:</strong> ';
  history.appendChild(aiDiv);

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split('\n');
    buffer = lines.pop();
    for (const line of lines) {
      if (line.startsWith('data: ')) {
        try {
          const d = JSON.parse(line.slice(6));
          if (d.type === 'chunk') aiDiv.innerHTML += esc(d.text);
          if (d.type === 'file_write' && editorInstance && d.filename === currentEditorFile) {
            editorInstance.setValue(d.content);
          }
          if (d.type === 'done' && status) status.textContent = '';
        } catch(_) {}
      }
    }
  }
  if (status) status.textContent = '';
  history.scrollTop = history.scrollHeight;
}

// ── Trigger Editor ───────────────────────────────────────────────────────────
function openTriggerEditor(taskID, triggerJSON) {
  let trigger = {};
  try { trigger = JSON.parse(triggerJSON); } catch(_) {}

  const edMount = document.getElementById('trigger-editor-mount');
  if (!edMount) return;

  let type = 'manual';
  if (trigger.Cron || trigger.cron) type = 'cron';
  else if (trigger.Webhook || trigger.webhook) type = 'webhook';
  else if (trigger.Chain || trigger.chain) type = 'chain';
  else if (trigger.Daemon || trigger.daemon) type = 'daemon';

  edMount.innerHTML = `
    <div class="card" style="margin-bottom:1rem">
      <h2 style="margin-bottom:0.75rem">Edit Trigger</h2>
      <div style="display:flex;gap:0.5rem;margin-bottom:1rem;flex-wrap:wrap">
        ${['manual','cron','webhook','chain','daemon'].map(t => `<button class="btn btn-sm${t===type?'':' secondary'}" onclick="triggerSelectType('${t}')">${t}</button>`).join('')}
      </div>
      <div id="trigger-fields"></div>
      <div style="display:flex;gap:0.5rem;margin-top:1rem">
        <button class="btn" onclick="saveTrigger('${taskID}')">Save</button>
        <button class="btn secondary" onclick="document.getElementById('trigger-editor-mount').innerHTML=''">Cancel</button>
      </div>
    </div>`;

  triggerSelectType(type, trigger);
}

function triggerSelectType(type, existing = {}) {
  const fields = document.getElementById('trigger-fields');
  if (!fields) return;
  let html = '';
  const cron = existing.Cron || existing.cron || '* * * * *';
  const webhook = existing.Webhook || existing.webhook || '/hooks/'+currentTaskID;
  const chainFrom = (existing.Chain || existing.chain || {}).From || (existing.Chain || existing.chain || {}).from || '';
  const restart = existing.Restart || existing.restart || 'always';
  if (type === 'cron') html = `<label>Cron expression<br><input id="trig-cron" class="input" value="${esc(cron)}" style="font-family:monospace;width:100%;margin-top:0.25rem"></label>`;
  if (type === 'webhook') html = `<label>Path<br><input id="trig-webhook" class="input" value="${esc(webhook)}" style="width:100%;margin-top:0.25rem"></label>`;
  if (type === 'chain') html = `<label>From task ID<br><input id="trig-from" class="input" value="${esc(chainFrom)}" style="width:100%;margin-top:0.25rem"></label>`;
  if (type === 'daemon') html = `<label>Restart policy<br><select id="trig-restart" class="input" style="margin-top:0.25rem"><option value="always"${restart==='always'?' selected':''}>always</option><option value="on-failure"${restart==='on-failure'?' selected':''}>on-failure</option><option value="never"${restart==='never'?' selected':''}>never</option></select></label>`;
  fields.innerHTML = html;
  fields.dataset.type = type;
}

async function saveTrigger(taskID) {
  const fields = document.getElementById('trigger-fields');
  const type = fields ? fields.dataset.type : 'manual';
  const body = { type };
  if (type === 'cron') body.cron = document.getElementById('trig-cron')?.value;
  if (type === 'webhook') body.webhook = document.getElementById('trig-webhook')?.value;
  if (type === 'chain') body.from = document.getElementById('trig-from')?.value;
  if (type === 'daemon') body.restart = document.getElementById('trig-restart')?.value;
  try {
    await post(`/api/tasks/${taskID}/trigger`, body);
    document.getElementById('trigger-editor-mount').innerHTML = '';
  } catch(e) { alert('Save failed: '+e.message); }
}

// ── Run Detail page ─────────────────────────────────────────────────────────
let runLogCleanup = null;

async function renderRunDetail(runID) {
  if (runLogCleanup) { runLogCleanup(); runLogCleanup = null; }
  mount('<div class="meta">Loading…</div>');

  let run, logs;
  try {
    [run, logs] = await Promise.all([get(`/api/runs/${runID}`), get(`/api/runs/${runID}/logs`)]);
  } catch(e) { mount(`<p style="color:red">Error: ${esc(e.message)}</p>`); return; }

  const taskName = run.task_name || run.TaskName || run.task_id || run.TaskID;
  const taskID = run.task_id || run.TaskID;
  const status = run.Status || run.status;
  const isRunning = status === 'running';

  let outputSection = '';
  const otype = run.OutputContentType || run.output_content_type;
  const ocontent = run.OutputContent || run.output_content;
  const retval = run.ReturnValue || run.return_value;
  if (otype) {
    outputSection = `
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:.5rem">
      <h2 style="margin:0">Output</h2>
      <a href="/runs/${runID}/result" target="_blank" style="font-size:.85rem">open full page ↗</a>
    </div>
    <div class="card" style="padding:0">
      ${otype === 'text/html'
        ? `<iframe srcdoc="${escAttr(ocontent)}" sandbox="allow-scripts allow-same-origin" style="width:100%;border:none;border-radius:6px;display:block" onload="this.style.height=(this.contentDocument.body.scrollHeight+32)+'px'"></iframe>`
        : `<pre style="margin:0;border-radius:6px">${esc(ocontent||'')}</pre>`}
    </div>`;
  } else if (retval) {
    let displayRV = esc(retval);
    try { displayRV = esc(JSON.stringify(JSON.parse(retval), null, 2)); } catch(_) {}
    outputSection = `<h2>Return value</h2><div class="card" style="padding:0"><pre style="margin:0;border-radius:6px">${displayRV}</pre></div>`;
  }

  const logLines = (logs||[]).map(l => `<span data-log-id="${l.id}">[${esc(l.level)}] ${fmtTime(l.ts)} ${esc(l.message)}\n</span>`).join('');

  const startedAt = run.StartedAt || run.started_at;
  const finishedAt = run.FinishedAt || run.finished_at;
  const triggerSource = run.TriggerSource || run.trigger_source;

  mount(`
    <div style="margin-bottom:1rem">
      <a href="/tasks/${taskID}" onclick="navigate('/tasks/${taskID}');return false">← ${esc(taskName)}</a>
    </div>
    <div id="run-status-card" class="card">
      <div style="display:flex;gap:1rem;align-items:center;flex-wrap:wrap">
        <span class="badge badge-${status}" id="run-badge">${status}</span>
        <strong>${esc(taskName)}</strong>
        ${triggerSource ? `<span class="meta badge badge-manual">${esc(triggerSource)}</span>` : ''}
        <span class="meta">Run <code>${runID.slice(0,8)}</code></span>
        <span class="meta">Started ${fmtTime(startedAt)}</span>
        ${finishedAt ? `<span class="meta" id="run-duration">${fmtDuration(startedAt, finishedAt)}</span>` : '<span class="meta" id="run-duration">running…</span>'}
        ${isRunning ? `<button class="btn" style="background:#dc3545;margin-left:auto" id="kill-btn" onclick="killRun('${runID}')">Kill</button>` : ''}
      </div>
    </div>

    ${outputSection}

    <h2>Logs</h2>
    <pre id="log-output" style="max-height:600px;overflow-y:auto">${logLines}</pre>`);

  const logEl = document.getElementById('log-output');
  if (logEl) logEl.scrollTop = logEl.scrollHeight;

  if (isRunning) {
    const offLog = wsOn('run:log', d => {
      if (d.runID !== runID) return;
      const logEl = document.getElementById('log-output');
      if (!logEl) { offLog(); return; }
      const span = document.createElement('span');
      span.textContent = `[${d.level}] ${new Date(d.ts).toLocaleTimeString()} ${d.message}\n`;
      logEl.appendChild(span);
      logEl.scrollTop = logEl.scrollHeight;
    });

    const offFinished = wsOn('run:finished', d => {
      if (d.runID !== runID) return;
      offLog();
      offFinished();
      const badge = document.getElementById('run-badge');
      if (badge) { badge.className = `badge badge-${d.status}`; badge.textContent = d.status; }
      const killBtn = document.getElementById('kill-btn');
      if (killBtn) killBtn.remove();
      const dur = document.getElementById('run-duration');
      if (dur) dur.textContent = (d.durationMs/1000).toFixed(1)+'s';
      if (d.outputContentType) {
        setTimeout(() => navigate(`/runs/${runID}`, false), 200);
      }
    });

    runLogCleanup = () => { offLog(); offFinished(); };
  }
}

async function killRun(runID) {
  if (!confirm('Kill this run?')) return;
  try { await post(`/api/runs/${runID}/kill`); } catch(e) { alert('Kill failed: '+e.message); }
}

// ── Config page ─────────────────────────────────────────────────────────────
async function renderConfig() {
  mount('<div class="meta">Loading…</div>');
  let cfg, raw;
  try {
    [cfg, raw] = await Promise.all([get('/api/config'), get('/api/config/raw')]);
  } catch(e) { mount(`<p style="color:red">Error: ${esc(e.message)}</p>`); return; }

  const ai = cfg.AI || cfg.ai || {};
  const srv = cfg.Server || cfg.server || {};
  const db = cfg.Database || cfg.database || {};
  const sources = cfg.Sources || cfg.sources || [];
  const tray = srv.Tray != null ? srv.Tray : (srv.tray != null ? srv.tray : true);

  const srcRows = sources.length ? sources.map((s, i) => `
    <tr>
      <td><span class="badge badge-manual">${esc(s.Type||s.type||'')}</span></td>
      <td style="word-break:break-all">${esc(s.Path||s.path||s.URL||s.url||'')}</td>
      <td class="meta">${s.Type==='git'||s.type==='git' ? `branch: ${esc(s.Branch||s.branch||'')}` : `watch: ${s.Watch||s.watch||false}`}</td>
      <td><button class="btn btn-sm" style="background:#c0392b" onclick="removeSource(${i})">Remove</button></td>
    </tr>`).join('') : `<tr><td colspan="4" class="meta" style="text-align:center">No sources configured.</td></tr>`;

  mount(`
    <h1>Configuration</h1>

    <style>
    .cfg-form input,.cfg-form select{background:#2a2a3e;color:#cdd6f4;border:1px solid #444;border-radius:4px;padding:0.35rem 0.5rem;font-size:0.85rem;width:100%;box-sizing:border-box}
    .cfg-form label{font-size:0.78rem;color:#888;display:block;margin-bottom:0.25rem}
    .cfg-form .field{margin-bottom:0.75rem}
    .cfg-form .hint{font-size:0.72rem;color:#666;margin-top:0.2rem}
    </style>

    <div class="card">
      <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.75rem">
        <h2 style="margin:0">AI</h2><span id="ai-status" style="font-size:0.82rem;color:#888"></span>
      </div>
      <div class="cfg-form">
        <div class="field"><label>Endpoint (base URL)</label>
          <input id="ai-base-url" value="${esc(ai.BaseURL||ai.base_url||'')}" placeholder="leave blank for OpenAI">
          <div class="hint">OpenAI: leave blank &nbsp;|&nbsp; Claude: https://api.anthropic.com/v1 &nbsp;|&nbsp; Ollama: http://localhost:11434/v1</div>
        </div>
        <div class="field"><label>Model</label>
          <input id="ai-model" value="${esc(ai.Model||ai.model||'')}" placeholder="gpt-4o">
        </div>
        <div class="field"><label>API key env var</label>
          <input id="ai-key-env" value="${esc(ai.APIKeyEnv||ai.api_key_env||'')}" placeholder="OPENAI_API_KEY">
        </div>
        <div class="field"><label>API key (direct value)</label>
          <input id="ai-key-val" type="password" placeholder="paste to set; leave blank to keep current">
        </div>
        <button class="btn" onclick="saveAI()">&#128190; Save AI settings</button>
      </div>
    </div>

    <div class="card">
      <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.75rem">
        <h2 style="margin:0">Server</h2><span id="srv-status" style="font-size:0.82rem;color:#888"></span>
      </div>
      <div class="cfg-form">
        <div class="field"><label>Port</label>
          <input value="${srv.Port||srv.port||''}" disabled style="color:#666;cursor:not-allowed">
          <div class="hint">Changing port requires restart; edit dicode.yaml directly.</div>
        </div>
        <div class="field"><label>Log level</label>
          <select id="srv-log-level">
            ${['debug','info','warn','error'].map(l=>`<option value="${l}"${(cfg.LogLevel||cfg.log_level||'info')===l?' selected':''}>${l}</option>`).join('')}
          </select>
        </div>
        <div class="field"><label>System tray icon</label>
          <select id="srv-tray">
            <option value="true"${tray?' selected':''}>Enabled</option>
            <option value="false"${!tray?' selected':''}>Disabled</option>
          </select>
          <div class="hint">Takes effect on next restart.</div>
        </div>
        <div class="field"><label>Secrets passphrase</label>
          <input id="srv-secret" type="password" placeholder="leave blank to keep current">
        </div>
        <button class="btn" onclick="saveServer()">&#128190; Save server settings</button>
      </div>
    </div>

    <div class="card">
      <h2>Database</h2>
      <table><tbody>
        <tr><th>Type</th><td>${esc(db.Type||db.type||'sqlite')}</td></tr>
        ${db.Path||db.path ? `<tr><th>Path</th><td>${esc(db.Path||db.path)}</td></tr>` : ''}
        ${db.URLEnv||db.url_env ? `<tr><th>URL env</th><td>${esc(db.URLEnv||db.url_env)}</td></tr>` : ''}
      </tbody></table>
    </div>

    <div class="card">
      <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.75rem">
        <h2 style="margin:0">Sources (${sources.length})</h2>
        <span id="src-status" style="font-size:0.82rem;color:#888"></span>
      </div>
      <table style="margin-bottom:1rem"><thead><tr><th>Type</th><th>Path / URL</th><th>Details</th><th></th></tr></thead>
        <tbody id="sources-tbody">${srcRows}</tbody>
      </table>
      <details>
        <summary style="cursor:pointer;font-size:0.85rem;color:#7c3aed;user-select:none">+ Add source</summary>
        <div class="cfg-form" style="margin-top:0.75rem">
          <div class="field"><label>Type</label>
            <select id="new-src-type" onchange="toggleSrcFields()">
              <option value="local">local</option><option value="git">git</option>
            </select>
          </div>
          <div id="src-local-fields">
            <div class="field"><label>Directory path</label><input id="new-src-path" placeholder="/home/you/tasks"></div>
          </div>
          <div id="src-git-fields" style="display:none">
            <div class="field"><label>Repository URL</label><input id="new-src-url" placeholder="https://github.com/you/tasks.git"></div>
            <div class="field"><label>Branch</label><input id="new-src-branch" placeholder="main"></div>
            <div class="field"><label>Auth token env var (optional)</label><input id="new-src-token-env" placeholder="GITHUB_TOKEN"></div>
          </div>
          <button class="btn" onclick="addSource()">Add source</button>
        </div>
      </details>
    </div>

    <div class="card">
      <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.75rem">
        <h2 style="margin:0">Raw YAML</h2><span id="config-status" style="font-size:0.82rem;color:#888"></span>
      </div>
      <div id="config-monaco" style="height:400px;border-radius:4px;overflow:hidden;margin-bottom:0.75rem"></div>
      <button class="btn" onclick="saveConfig()">&#128190; Save</button>
    </div>`);

  require.config({ paths: { vs: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs' } });
  require(['vs/editor/editor.main'], () => {
    window._configEditor = monaco.editor.create(document.getElementById('config-monaco'), {
      value: raw.content || '',
      language: 'yaml',
      theme: 'vs-dark',
      fontSize: 13,
      minimap: { enabled: false },
    });
  });
}

function toggleSrcFields() {
  const t = document.getElementById('new-src-type').value;
  document.getElementById('src-local-fields').style.display = t === 'local' ? '' : 'none';
  document.getElementById('src-git-fields').style.display = t === 'git' ? '' : 'none';
}

async function saveAI() {
  const st = document.getElementById('ai-status');
  if (st) st.textContent = 'Saving…';
  try {
    await post('/api/settings/ai', {
      base_url: document.getElementById('ai-base-url').value,
      model: document.getElementById('ai-model').value,
      api_key_env: document.getElementById('ai-key-env').value,
      api_key: document.getElementById('ai-key-val').value,
    });
    if (st) { st.textContent = 'Saved ✓'; setTimeout(() => { if(st) st.textContent=''; }, 2000); }
  } catch(e) { if (st) st.textContent = 'Error: '+e.message; }
}

async function saveServer() {
  const st = document.getElementById('srv-status');
  if (st) st.textContent = 'Saving…';
  try {
    const trayVal = document.getElementById('srv-tray').value === 'true';
    await post('/api/settings/server', {
      log_level: document.getElementById('srv-log-level').value,
      tray: trayVal,
      secret: document.getElementById('srv-secret').value || undefined,
    });
    if (st) { st.textContent = 'Saved ✓'; setTimeout(() => { if(st) st.textContent=''; }, 2000); }
  } catch(e) { if (st) st.textContent = 'Error: '+e.message; }
}

async function removeSource(idx) {
  if (!confirm('Remove this source?')) return;
  const st = document.getElementById('src-status');
  try {
    await del(`/api/settings/sources/${idx}`);
    renderConfig();
  } catch(e) { if (st) st.textContent = 'Error: '+e.message; }
}

async function addSource() {
  const type = document.getElementById('new-src-type').value;
  const st = document.getElementById('src-status');
  const body = { type };
  if (type === 'local') {
    body.path = document.getElementById('new-src-path').value;
  } else {
    body.url = document.getElementById('new-src-url').value;
    body.branch = document.getElementById('new-src-branch').value || 'main';
    body.token_env = document.getElementById('new-src-token-env').value;
  }
  try {
    await post('/api/settings/sources', body);
    renderConfig();
  } catch(e) { if (st) st.textContent = 'Error: '+e.message; }
}

async function saveConfig() {
  if (!window._configEditor) return;
  const st = document.getElementById('config-status');
  if (st) st.textContent = 'Saving…';
  try {
    await post('/api/config/raw', { content: window._configEditor.getValue() });
    if (st) { st.textContent = 'Saved ✓'; setTimeout(() => { if(st) st.textContent=''; }, 2000); }
  } catch(e) { if (st) st.textContent = 'Error: '+e.message; }
}

// ── Secrets page ─────────────────────────────────────────────────────────────
async function renderSecrets() {
  mount('<div class="meta">Loading…</div>');
  let secrets;
  try { secrets = await get('/api/secrets'); } catch(e) {
    mount(`
      <h1>Secrets</h1>
      <div class="card" style="max-width:400px">
        <h2>Unlock Secrets</h2>
        <p class="meta" style="margin-bottom:1rem">Enter your master password to view and edit secrets.</p>
        <input type="password" id="secrets-pw" placeholder="Master password" class="input" style="width:100%;margin-bottom:0.5rem" onkeydown="if(event.key==='Enter')unlockSecrets()">
        <button class="btn" onclick="unlockSecrets()">Unlock</button>
        <span id="secrets-status" style="margin-left:0.5rem;font-size:0.85rem;color:red"></span>
      </div>`);
    return;
  }

  const rows = (secrets||[]).map(k => `
    <tr>
      <td><code>${esc(k)}</code></td>
      <td style="text-align:right">
        <button class="btn btn-sm" style="background:#dc3545" onclick="deleteSecret('${esc(k)}')">Delete</button>
      </td>
    </tr>`).join('');

  mount(`
    <h1>Secrets</h1>
    <div style="display:flex;gap:0.5rem;margin-bottom:1rem;flex-wrap:wrap">
      <button class="btn btn-sm secondary" onclick="lockSecrets()">Lock</button>
    </div>
    <div class="card" style="margin-bottom:1rem">
      <h2 style="margin-bottom:0.75rem">Add Secret</h2>
      <div style="display:flex;gap:0.5rem;flex-wrap:wrap">
        <input id="secret-key" placeholder="KEY_NAME" class="input" style="font-family:monospace">
        <input id="secret-value" type="password" placeholder="value" class="input" style="flex:1;min-width:200px">
        <button class="btn" onclick="setSecret()">Add</button>
      </div>
    </div>
    <table>
      <thead><tr><th>Key</th><th></th></tr></thead>
      <tbody>${rows||'<tr><td colspan="2" style="text-align:center;color:#888">No secrets stored.</td></tr>'}</tbody>
    </table>`);
}

async function unlockSecrets() {
  const pw = document.getElementById('secrets-pw')?.value;
  const st = document.getElementById('secrets-status');
  try {
    await post('/api/secrets/unlock', { password: pw });
    renderSecrets();
  } catch(e) { if (st) st.textContent = 'Incorrect password'; }
}

async function lockSecrets() {
  try { await post('/api/secrets/lock'); renderSecrets(); } catch(e) {}
}

async function setSecret() {
  const key = document.getElementById('secret-key')?.value;
  const value = document.getElementById('secret-value')?.value;
  if (!key) return;
  try {
    await post('/api/secrets', { key, value });
    renderSecrets();
  } catch(e) { alert('Failed: '+e.message); }
}

async function deleteSecret(key) {
  if (!confirm(`Delete secret "${key}"?`)) return;
  try { await del(`/api/secrets/${key}`); renderSecrets(); } catch(e) { alert('Failed: '+e.message); }
}

// ── 404 ───────────────────────────────────────────────────────────────────────
function renderNotFound() {
  mount('<h1>404 — Page not found</h1><a href="/" onclick="navigate(\'/\');return false">← Back to tasks</a>');
}

// ── Utilities ─────────────────────────────────────────────────────────────────
function esc(str) {
  return String(str||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}
function escAttr(str) {
  return esc(str).replace(/'/g,'&#39;');
}
function fmtTime(ts) {
  if (!ts) return '—';
  return new Date(typeof ts === 'number' ? ts : ts).toLocaleString();
}
function fmtDuration(start, end) {
  if (!end) return '—';
  const ms = new Date(end) - new Date(start);
  return (ms/1000).toFixed(1)+'s';
}

// ── Service Worker ────────────────────────────────────────────────────────────
let swReg = null;
function registerSW() {
  if (!('serviceWorker' in navigator)) return;
  navigator.serviceWorker.register('/sw.js', { scope: '/' }).then(reg => { swReg = reg; }).catch(() => {});
}

// ── Init ───────────────────────────────────────────────────────────────────────
// Routes
route(/^\/tasks\/([^/]+)$/, renderTaskDetail);
route(/^\/runs\/([^/]+)$/, renderRunDetail);
route(/^\/config$/, renderConfig);
route(/^\/secrets$/, renderSecrets);
route(/^\/$/, renderTaskList);

// Event bindings
document.getElementById('logbar-header')?.addEventListener('click', toggleLogBar);
document.getElementById('notif-bell')?.addEventListener('click', () => {
  inboxOpen = !inboxOpen;
  const p = document.getElementById('notif-panel');
  if (p) p.style.display = inboxOpen ? 'block' : 'none';
  if (inboxOpen) { unread = 0; updateBadge(); renderInbox(); }
});
document.getElementById('notif-clear')?.addEventListener('click', () => { saveNotifs([]); renderInbox(); });

// WS run events → notifications + task list badges
wsOn('run:finished', d => {
  addNotif(d);
  const badge = document.querySelector(`#task-last-status-${d.taskID}`);
  if (badge) badge.innerHTML = `<span class="badge badge-${d.status}">${d.status}</span>`;
});

// WS log lines → log bar
wsOn('log:line', d => logBarAppend(d.line));

// Start
wsConnect();
registerSW();
renderInbox();
render(location.pathname);
