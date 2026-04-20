import { LitElement, html } from 'https://esm.sh/lit@3';
import { unsafeHTML } from 'https://esm.sh/lit@3/directives/unsafe-html.js';
import { marked } from 'https://esm.sh/marked@14';
import { get, post } from '../lib/api.js';
import { wsOn } from '../lib/ws.js';
import { navigate } from '../lib/router.js';
import { fmtTime, fmtDuration } from '../lib/utils.js';
import { relayHookBaseURL, webhookURL } from '../lib/config.js';
import { monacoTheme } from '../lib/theme.js';

marked.use({ gfm: true, breaks: true });

class DcTaskDetail extends LitElement {
  createRenderRoot() { return this; } // light DOM — Monaco needs direct DOM access

  static properties = {
    taskid: { type: String },
    _task:            { state: true },
    _runs:            { state: true },
    _error:           { state: true },
    _triggerOpen:     { state: true },
    _triggerType:     { state: true },
    _editorOpen:      { state: true },
    _editorStatus:    { state: true },
    _aiOpen:          { state: true },
    _aiHistory:       { state: true },
    _aiStatus:        { state: true },
    _aiSessionId:     { state: true },
    _currentFile:     { state: true },
  };

  constructor() {
    super();
    this._task = null; this._runs = null; this._error = null;
    this._triggerOpen = false; this._triggerType = 'manual';
    this._editorOpen = false; this._editorStatus = ''; this._currentFile = null;
    this._aiOpen = false; this._aiHistory = []; this._aiStatus = ''; this._aiSessionId = '';
    this._editor = null;
    this._relayBase = '';
    this._offStarted = null; this._offFinished = null;
  }

  updated(changed) {
    if (changed.has('taskid') && changed.get('taskid') !== undefined) {
      this._cleanup();
      this._load();
    }
  }

  connectedCallback() {
    super.connectedCallback();
    this._load();
    this._onThemeChange = () => {
      if (window.monaco) window.monaco.editor.setTheme(monacoTheme());
    };
    window.addEventListener('dicode-theme-change', this._onThemeChange);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    window.removeEventListener('dicode-theme-change', this._onThemeChange);
    this._cleanup();
  }

  _cleanup() {
    this._offStarted?.(); this._offStarted = null;
    this._offFinished?.(); this._offFinished = null;
    if (this._editor) { this._editor.dispose(); this._editor = null; }
  }

  async _load() {
    if (!this.taskid) return;
    this._task = null; this._runs = null; this._error = null;
    this._editorOpen = false; this._triggerOpen = false;
    try {
      const [task, runs, base] = await Promise.all([
        get(`/api/tasks/${encodeURIComponent(this.taskid)}`),
        get(`/api/tasks/${encodeURIComponent(this.taskid)}/runs?limit=20`),
        relayHookBaseURL(),
      ]);
      this._task = task;
      this._relayBase = base;
      this._runs = runs || [];
      const t = task.trigger || task.Trigger || {};
      if (t.cron || t.Cron) this._triggerType = 'cron';
      else if (t.webhook || t.Webhook) this._triggerType = 'webhook';
      else if (t.chain || t.Chain) this._triggerType = 'chain';
      else if (t.daemon || t.Daemon) this._triggerType = 'daemon';
      else this._triggerType = 'manual';
    } catch(e) {
      this._error = e.message;
      return;
    }

    this._offStarted = wsOn('run:started', d => {
      if (d.taskID !== this.taskid) return;
      this._runs = [{ ID: d.runID, Status: 'running', StartedAt: new Date().toISOString() }, ...(this._runs || [])];
    });
    this._offFinished = wsOn('run:finished', d => {
      if (d.taskID !== this.taskid) return;
      const finishedAt = new Date().toISOString();
      const existing = (this._runs || []).find(r => r.ID === d.runID);
      if (existing) {
        this._runs = this._runs.map(r => r.ID === d.runID
          ? { ...r, Status: d.status, FinishedAt: finishedAt, OutputContentType: d.outputContentType, ReturnValue: d.returnValue }
          : r);
      } else {
        this._runs = [{ ID: d.runID, Status: d.status, StartedAt: finishedAt, FinishedAt: finishedAt, OutputContentType: d.outputContentType, ReturnValue: d.returnValue }, ...(this._runs || [])];
      }
    });
  }

  async _run() {
    try {
      const r = await post(`/api/tasks/${encodeURIComponent(this.taskid)}/run`);
      navigate(`/runs/${r.runId}`);
    } catch(e) { alert('Failed: ' + e.message); }
  }

  _openEditor() {
    this._editorOpen = true;
    this._currentFile = this._task?.script_file || 'task.ts';
    this.updateComplete.then(() => this._loadEditorFile(this._currentFile));
  }

  _closeEditor() {
    if (this._editor) { this._editor.dispose(); this._editor = null; }
    this._editorOpen = false;
  }

  async _loadEditorFile(filename) {
    this._currentFile = filename;
    try {
      const content = await fetch(`/api/tasks/${encodeURIComponent(this.taskid)}/files/${filename}`)
        .then(r => r.ok ? r.text() : Promise.reject(new Error('not found')));
      const lang = filename.endsWith('.ts') ? 'typescript' : filename.endsWith('.py') ? 'python' : 'javascript';
      const container = this.querySelector('#monaco-container');
      if (!container) return;
      if (!this._editor) {
        require.config({ paths: { vs: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs' } });
        require(['vs/editor/editor.main'], async () => {
          const dts = await fetch('/api/sdk/types').then(r => r.ok ? r.text() : '');
          if (dts) {
            monaco.languages.typescript.typescriptDefaults.addExtraLib(dts, 'file:///dicode-sdk.d.ts');
            monaco.languages.typescript.javascriptDefaults.addExtraLib(dts, 'file:///dicode-sdk.d.ts');
          }
          this._editor = monaco.editor.create(container, {
            value: content, language: lang, theme: monacoTheme(),
            fontSize: 13, minimap: { enabled: false }, scrollBeyondLastLine: false,
          });
        });
      } else {
        const model = this._editor.getModel();
        if (model) { monaco.editor.setModelLanguage(model, lang); this._editor.setValue(content); }
      }
    } catch(e) { this._editorStatus = 'Error: ' + e.message; }
  }

  async _saveEditor() {
    if (!this._editor || !this._currentFile) return;
    this._editorStatus = 'Saving…';
    try {
      await fetch(`/api/tasks/${encodeURIComponent(this.taskid)}/files/${this._currentFile}`, {
        method: 'POST', headers: { 'Content-Type': 'text/plain' }, body: this._editor.getValue(),
      });
      this._editorStatus = 'Saved ✓';
      setTimeout(() => { this._editorStatus = ''; }, 2000);
    } catch(e) { this._editorStatus = 'Error: ' + e.message; }
  }

  async _saveTrigger() {
    const type = this._triggerType;
    const body = { type };
    if (type === 'cron')    body.cron    = this.querySelector('#trig-cron')?.value;
    if (type === 'webhook') body.webhook = this.querySelector('#trig-webhook')?.value;
    if (type === 'chain')   body.from    = this.querySelector('#trig-from')?.value;
    if (type === 'daemon')  body.restart = this.querySelector('#trig-restart')?.value;
    try {
      await post(`/api/tasks/${encodeURIComponent(this.taskid)}/trigger`, body);
      this._triggerOpen = false;
      this._task = await get(`/api/tasks/${encodeURIComponent(this.taskid)}`);
    } catch(e) { alert('Save failed: ' + e.message); }
  }

  async _aiSend() {
    const input = this.querySelector('#ai-input');
    if (!input) return;
    const msg = input.value.trim();
    if (!msg) return;
    input.value = '';
    this._aiHistory = [...this._aiHistory, { role: 'user', text: msg }];
    this._aiStatus = 'Thinking…';
    const aiMsg = { role: 'ai', text: '' };
    this._aiHistory = [...this._aiHistory, aiMsg];

    // Path is coupled to the `dicodai` buildin taskset entry
    // (tasks/buildin/taskset.yaml). Fork and override there if you need
    // to point this UI at a different provider/preset.
    // Agent replies are text-only — paste code back into the editor manually.
    const ctx = {
      task_id: this.taskid,
    };
    if (this._editor && this._currentFile) {
      ctx.current_file = this._currentFile;
      ctx.current_file_content = this._editor.getValue();
    }

    let reply = '';
    try {
      const res = await fetch('/hooks/ai/dicodai', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          prompt: msg,
          session_id: this._aiSessionId || '',
          ...ctx,
        }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      this._aiSessionId = data.session_id || this._aiSessionId || '';
      reply = data.reply == null ? '(no reply — check dicodai provider config)' : String(data.reply);
    } catch (e) {
      reply = `Error: ${e.message}`;
    }
    aiMsg.text = reply;
    this._aiHistory = [...this._aiHistory];
    this._aiStatus = '';
    this.updateComplete.then(() => {
      const h = this.querySelector('#ai-history');
      if (h) h.scrollTop = h.scrollHeight;
    });
  }

  _triggerFields() {
    const t = this._task?.trigger || this._task?.Trigger || {};
    const type = this._triggerType;
    const cron    = t.cron    || t.Cron    || '* * * * *';
    const webhook = t.webhook || t.Webhook || `/hooks/${this.taskid}`;
    const chainFrom = (t.chain || t.Chain || {}).from || (t.chain || t.Chain || {}).From || '';
    const restart = t.restart || t.Restart || 'always';

    if (type === 'cron') return html`
      <label>Cron expression<br>
        <input id="trig-cron" class="input" .value=${cron} style="font-family:monospace;width:100%;margin-top:0.25rem">
      </label>`;
    if (type === 'webhook') {
      const fullURL = this._relayBase ? webhookURL(this._relayBase, webhook) : '';
      return html`
      <label>Path<br>
        <input id="trig-webhook" class="input" .value=${webhook} style="width:100%;margin-top:0.25rem">
      </label>
      ${fullURL ? html`<div style="margin-top:0.5rem;font-size:0.85rem;color:var(--muted)">
        Relay URL: <code style="user-select:all;word-break:break-all">${fullURL}</code>
      </div>` : ''}`;
    }
    if (type === 'chain') return html`
      <label>From task ID<br>
        <input id="trig-from" class="input" .value=${chainFrom} style="width:100%;margin-top:0.25rem">
      </label>`;
    if (type === 'daemon') return html`
      <label>Restart policy<br>
        <select id="trig-restart" class="input" style="margin-top:0.25rem">
          <option value="always"     ?selected=${restart === 'always'}>always</option>
          <option value="on-failure" ?selected=${restart === 'on-failure'}>on-failure</option>
          <option value="never"      ?selected=${restart === 'never'}>never</option>
        </select>
      </label>`;
    return html`<p class="meta">No configuration needed for manual trigger.</p>`;
  }

  render() {
    if (this._error) return html`<p style="color:red">Error: ${this._error}</p>`;
    if (!this._task) return html`<div class="meta">Loading…</div>`;

    const task = this._task;
    const scriptFile = task.script_file || 'task.ts';
    const testFile   = task.test_file   || scriptFile.replace(/\.(ts|js)$/, '.test.$1');

    return html`
      <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:var(--space-sm)">
        <h1 style="margin:0">${task.name}</h1>
        <button class="btn" @click=${() => this._run()}>&#9654; Run now</button>
        <button class="btn" style="background:var(--muted)" @click=${() => this._openEditor()}>&#9998; Edit code</button>
      </div>
      ${task.description ? html`<div class="task-desc">${unsafeHTML(marked.parse(task.description))}</div>` : ''}

      <div class="card" style="margin-bottom:var(--space-md);display:flex;align-items:center;gap:0.75rem">
        <span style="font-size:0.85rem"><strong>Trigger:</strong> ${task.trigger_label || 'manual'}</span>
        <button class="btn btn-sm" style="background:var(--muted);margin-left:auto"
          @click=${() => { this._triggerOpen = !this._triggerOpen; }}>&#9998; Edit trigger</button>
      </div>

      ${this._triggerOpen ? html`
        <div class="card" style="margin-bottom:var(--space-md)">
          <h2 style="margin-bottom:0.75rem">Edit Trigger</h2>
          <div style="display:flex;gap:var(--space-sm);margin-bottom:var(--space-md);flex-wrap:wrap">
            ${['manual','cron','webhook','chain','daemon'].map(t => html`
              <button class="btn btn-sm ${t === this._triggerType ? '' : 'secondary'}"
                @click=${() => { this._triggerType = t; }}>${t}</button>`)}
          </div>
          ${this._triggerFields()}
          <div style="display:flex;gap:var(--space-sm);margin-top:1rem">
            <button class="btn" @click=${() => this._saveTrigger()}>Save</button>
            <button class="btn secondary" @click=${() => { this._triggerOpen = false; }}>Cancel</button>
          </div>
        </div>` : ''}

      ${this._editorOpen ? html`
        <div class="card" style="margin-top:1.5rem;padding:0.75rem">
          <div style="display:flex;align-items:center;gap:var(--space-sm);margin-bottom:var(--space-sm);flex-wrap:wrap">
            <button class="btn btn-sm" @click=${() => this._loadEditorFile(scriptFile)}>${scriptFile}</button>
            ${task.test_exists ? html`
              <button class="btn btn-sm secondary" @click=${() => this._loadEditorFile(testFile)}>${testFile}</button>` : ''}
            <div style="margin-left:auto;display:flex;gap:var(--space-sm);align-items:center">
              <span style="font-size:0.8rem">${this._editorStatus}</span>
              <button class="btn btn-sm" @click=${() => this._saveEditor()}>&#128190; Save</button>
              <button class="btn btn-sm" style="background:var(--lavender)" @click=${() => { this._aiOpen = !this._aiOpen; }}>&#129302; AI</button>
              <button class="btn btn-sm secondary" @click=${() => this._closeEditor()}>✕ Close</button>
            </div>
          </div>
          <div style="display:flex;gap:0.75rem;align-items:stretch">
            <div id="monaco-container" style="flex:1;min-width:0;height:440px;border-radius:var(--radius-sm);overflow:hidden"></div>
            ${this._aiOpen ? html`
              <div style="width:360px;flex-shrink:0;display:flex;flex-direction:column;background:var(--bg-alt);border-radius:var(--radius-sm);border:1px solid var(--border);overflow:hidden">
                <div style="padding:var(--space-sm) 0.75rem;background:var(--bg-alt);border-bottom:1px solid var(--border)">
                  <span style="color:var(--sky);font-weight:600;font-size:0.85rem">&#129302; AI Task Dev</span>
                </div>
                <div id="ai-history" style="flex:1;overflow-y:auto;padding:0.75rem;font-size:0.8rem;color:var(--lavender);min-height:240px;max-height:300px;line-height:1.5">
                  ${this._aiHistory.map(m => html`
                    <div style="margin-bottom:var(--space-sm)">
                      <strong style="color:${m.role === 'user' ? 'var(--sky)' : 'var(--green)'}">${m.role === 'user' ? 'You' : 'AI'}:</strong>
                      ${m.text}
                    </div>`)}
                </div>
                <div style="padding:0.2rem 0.75rem;font-size:0.7rem;color:var(--muted);border-top:1px solid var(--border);min-height:1.4rem;font-family:monospace">
                  ${this._aiStatus}
                </div>
                <div style="padding:var(--space-sm);border-top:1px solid var(--border);display:flex;flex-direction:column;gap:0.4rem">
                  <textarea id="ai-input"
                    placeholder="Describe the task… (Ctrl+Enter to send)"
                    style="width:100%;background:var(--bg);color:var(--lavender);border:1px solid var(--border);border-radius:var(--radius-sm);padding:0.45rem 0.5rem;font-size:0.78rem;resize:none;height:72px;font-family:system-ui,sans-serif;outline:none;box-sizing:border-box"
                    @keydown=${e => { if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { this._aiSend(); e.preventDefault(); } }}>
                  </textarea>
                  <button class="btn btn-sm" style="background:var(--lavender)" @click=${() => this._aiSend()}>Send</button>
                </div>
              </div>` : ''}
          </div>
        </div>` : ''}

      <h2>Recent runs</h2>
      <table>
        <thead><tr><th>Run ID</th><th>Status</th><th>Started</th><th>Duration</th><th></th></tr></thead>
        <tbody>
          ${!this._runs?.length ? html`
            <tr><td colspan="5" style="text-align:center;color:var(--muted)">No runs yet.</td></tr>
          ` : this._runs.map(r => html`
            <tr>
              <td><a href="runs/${r.ID}">${r.ID.slice(0,8)}</a></td>
              <td><span class="badge badge-${r.Status}">${r.Status}</span></td>
              <td class="meta">${fmtTime(r.StartedAt)}</td>
              <td class="meta">${fmtDuration(r.StartedAt, r.FinishedAt)}</td>
              <td>${(r.OutputContentType || r.ReturnValue) ? html`
                <a href="/runs/${r.ID}/result" target="_blank"
                   class="btn btn-sm secondary">Result</a>` : ''}</td>
            </tr>`)}
        </tbody>
      </table>`;
  }
}

customElements.define('dc-task-detail', DcTaskDetail);
