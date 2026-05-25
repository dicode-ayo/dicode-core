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
    _showStages:      { state: true },
    _expanded:        { state: true }, // Set<runID> — top-level rows with children currently expanded
    _children:        { state: true }, // Map<parentRunID, Run[]> — lazily-fetched child rows
  };

  constructor() {
    super();
    this._task = null; this._runs = null; this._error = null;
    this._triggerOpen = false; this._triggerType = 'manual';
    this._editorOpen = false; this._editorStatus = ''; this._currentFile = null;
    this._aiOpen = false; this._aiHistory = []; this._aiStatus = ''; this._aiSessionId = '';
    this._showStages = false;
    this._expanded = new Set();
    this._children = new Map();
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

    // /api/ai/chat forwards to the task named by ai.task in dicode.yaml
    // (default: buildin/dicodai). Point ai.task at any preset to swap
    // providers, skills, or model without touching this component.
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
      const res = await fetch('/api/ai/chat', {
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

  // ── Run-list grouping (#114) ────────────────────────────────────────────
  // The flat list of runs returned by /api/tasks/{id}/runs is collapsed by
  // two rules so the user sees one row per logical event:
  //
  //   1. Hide rows whose ParentRunID is set — those are stage runs (a
  //      pipeline stage, or a sub-task fired via dicode.run_task). A toggle
  //      restores the flat view for debugging.
  //   2. Collapse consecutive top-level rows with the same non-empty Group
  //      into one row ("N runs in this session, last 3m ago"). Tasks tag
  //      themselves via dicode.set_group() — see the ai-agent buildin (#112).
  //
  // The result is a list of "items" where each item is either:
  //   { kind: 'single', run }              — one row
  //   { kind: 'group',  runs: [...] }      — collapse of consecutive same-group runs
  _buildRunItems() {
    const all = this._runs || [];
    if (this._showStages) return all.map(run => ({ kind: 'single', run }));

    const top = all.filter(r => !(r.ParentRunID || r.parent_run_id));
    const items = [];
    for (const run of top) {
      const group = run.Group || run.group;
      const last = items[items.length - 1];
      if (group && last && last.kind === 'group' && (last.runs[0].Group || last.runs[0].group) === group) {
        last.runs.push(run);
        continue;
      }
      if (group) {
        items.push({ kind: 'group', runs: [run] });
      } else {
        items.push({ kind: 'single', run });
      }
    }
    return items;
  }

  // _toggleExpand expands or collapses an inline children panel. On first
  // expand it lazily fetches /api/runs?parent=<id>; subsequent toggles flip
  // the visibility without re-fetching.
  async _toggleExpand(runID) {
    const next = new Set(this._expanded);
    if (next.has(runID)) {
      next.delete(runID);
      this._expanded = next;
      return;
    }
    next.add(runID);
    this._expanded = next;
    if (!this._children.has(runID)) {
      try {
        const kids = await get(`/api/runs?parent=${encodeURIComponent(runID)}&limit=50`);
        const cm = new Map(this._children);
        cm.set(runID, kids || []);
        this._children = cm;
      } catch (e) {
        const cm = new Map(this._children);
        cm.set(runID, []);
        this._children = cm;
      }
    }
  }

  _renderRunRow(r, opts) {
    const indent = opts?.indent || 0;
    const hint   = opts?.hint;
    const expandedKid = !!opts?.isChild;
    const padding = indent ? `padding-left:${indent * 1.5}rem` : '';
    const expanded = this._expanded.has(r.ID);
    const kids = this._children.get(r.ID) || [];
    const showExpand = !expandedKid; // sub-rows don't recurse further by default
    // registry.Run has no JSON tags, so the kind serializes as `Kind`
    // ("task" | "pipeline"); badge pipeline parents so operators can tell a
    // pipeline run (whose stage children expand below) from a plain task run.
    const isPipeline = (r.Kind || r.kind) === 'pipeline';
    return html`
      <tr>
        <td style=${padding}>
          ${showExpand ? html`<button class="btn btn-sm secondary"
            style="padding:0 .35rem;margin-right:.35rem;font-family:monospace"
            title="Show stage runs"
            @click=${() => this._toggleExpand(r.ID)}>${expanded ? '▾' : '▸'}</button>` : ''}
          <a href="runs/${r.ID}">${r.ID.slice(0,8)}</a>
          ${isPipeline ? html`<span class="badge" title="Pipeline run"
            style="margin-left:.5rem;font-size:0.7rem;background:rgba(137, 220, 235, .15);color:var(--sky)">pipeline</span>` : ''}
          ${hint ? html`<span class="meta" style="margin-left:.5rem">${hint}</span>` : ''}
        </td>
        <td><span class="badge badge-${r.Status}">${r.Status}</span></td>
        <td class="meta">${fmtTime(r.StartedAt)}</td>
        <td class="meta">${fmtDuration(r.StartedAt, r.FinishedAt)}</td>
        <td>${(r.OutputContentType || r.ReturnValue) ? html`
          <a href="/runs/${r.ID}/result" target="_blank"
             class="btn btn-sm secondary">Result</a>` : ''}</td>
      </tr>
      ${expanded ? (kids.length ? kids.map(kid => this._renderRunRow(kid, { indent: indent + 1, isChild: true })) : html`
        <tr><td colspan="5" class="meta" style="padding-left:${(indent + 1) * 1.5}rem">No sub-runs.</td></tr>
      `) : ''}`;
  }

  _renderGroupRow(item) {
    const head = item.runs[0];
    const group = head.Group || head.group;
    const expanded = this._expanded.has(`group:${group}`);
    const last = head; // newest first per ListRuns ORDER BY
    return html`
      <tr>
        <td>
          <button class="btn btn-sm secondary"
            style="padding:0 .35rem;margin-right:.35rem;font-family:monospace"
            title="Expand session"
            @click=${() => {
              const next = new Set(this._expanded);
              const key = `group:${group}`;
              next.has(key) ? next.delete(key) : next.add(key);
              this._expanded = next;
            }}>${expanded ? '▾' : '▸'}</button>
          <strong>${group}</strong>
          <span class="meta" style="margin-left:.5rem">${item.runs.length} runs</span>
        </td>
        <td><span class="badge badge-${last.Status}">${last.Status}</span></td>
        <td class="meta">last ${fmtTime(last.StartedAt)}</td>
        <td class="meta">—</td>
        <td></td>
      </tr>
      ${expanded ? item.runs.map(r => this._renderRunRow(r, { indent: 1 })) : ''}`;
  }

  // Renders the daemon lifecycle phase as a colored badge in the trigger
  // row. The engine reports a five-value enum (see pkg/trigger.DaemonState);
  // the mapping below keeps colors aligned with the surrounding UI palette:
  // green for healthy, yellow for transient, red for failure, muted for
  // resting. The two failure badges are deliberately distinguishable so
  // operators can tell "daemon body launch failed" (issue #318) from
  // "daemon body ran then crashed without an auto-restart" (issue #325).
  _renderDaemonState(state) {
    // Keep keys in sync with the DaemonState constants in
    // pkg/trigger/daemon_state.go — out-of-sync entries fall through
    // to the generic-text fallback below at render time. The wire value
    // "failed_after_preflight" is a retained enum value; its current
    // meaning is simply "daemon body launch failed".
    const STYLES = {
      running:                { bg: 'rgba(166, 227, 161, .15)', fg: 'var(--green)',  text: 'Running' },
      stopping:               { bg: 'rgba(249, 226, 175, .15)', fg: 'var(--yellow)', text: 'Stopping…' },
      failed_after_preflight: { bg: 'rgba(243, 139, 168, .15)', fg: 'var(--red)',    text: '⨯ Launch failed' },
      crashed:                { bg: 'rgba(243, 139, 168, .28)', fg: 'var(--red)',    text: '⨯ Crashed (no restart)' },
      stopped:                { bg: 'rgba(166, 173, 200, .15)', fg: 'var(--muted)',  text: 'Stopped' },
    };
    const s = STYLES[state] || { bg: 'rgba(166, 173, 200, .15)', fg: 'var(--muted)', text: state };
    return html`<span
      data-daemon-state=${state}
      title="Daemon state: ${state}"
      style="font-size:0.75rem;font-weight:600;padding:2px 8px;border-radius:9999px;background:${s.bg};color:${s.fg};white-space:nowrap"
      >${s.text}</span>`;
  }

  // Renders the ordered stages of a kind: PipelineTask. The PipelineDetail
  // payload embeds *task.PipelineTask, whose Stage struct has NO JSON tags, so
  // each element serializes with Go field-name casing (`Task`, `Overrides`);
  // we read both casings defensively. Stage task IDs are operator-controlled
  // and therefore rendered via lit `html` text bindings (auto-escaped), never
  // unsafeHTML. The terminal-stage daemon_state badge already renders in the
  // trigger card above, so it isn't duplicated here.
  _renderStages(task) {
    const stages = task.stages || task.Stages || [];
    if (!stages.length) {
      return html`
        <div class="card" style="margin-bottom:var(--space-md)">
          <h2 style="margin:0 0 0.5rem">Stages</h2>
          <p class="meta" style="margin:0">This pipeline has no stages.</p>
        </div>`;
    }
    return html`
      <div class="card" style="margin-bottom:var(--space-md)">
        <h2 style="margin:0 0 0.75rem">Stages</h2>
        <ol style="margin:0;padding-left:1.5rem;display:flex;flex-direction:column;gap:0.4rem">
          ${stages.map(s => {
            const id = s.Task || s.task || '(unknown)';
            const hasOverrides = !!(s.Overrides || s.overrides);
            return html`
              <li>
                <a href="tasks/${encodeURIComponent(id)}" style="font-family:monospace">${id}</a>
                ${hasOverrides ? html`<span class="badge" title="This stage applies overrides"
                  style="margin-left:.5rem;font-size:0.7rem;background:rgba(249, 226, 175, .15);color:var(--yellow)">overrides</span>` : ''}
              </li>`;
          })}
        </ol>
      </div>`;
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
    // A kind: PipelineTask has only a task.yaml — no script/test file — so the
    // code editor (Edit-code button + Monaco tab) is meaningless and would
    // 404 on save/load. Gate it on the presence of a script_file (kind: Task
    // detail always sets one; PipelineDetail never does) and double-guard on
    // the task kind so a future PipelineDetail field can't accidentally re-open
    // the affordance.
    const isPipeline = task.kind === 'PipelineTask';
    const hasEditor  = !isPipeline && !!task.script_file;

    return html`
      <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:var(--space-sm)">
        <h1 style="margin:0">${task.name}</h1>
        <button class="btn" @click=${() => this._run()}>&#9654; Run now</button>
        ${hasEditor ? html`<button class="btn" style="background:var(--muted)" @click=${() => this._openEditor()}>&#9998; Edit code</button>` : ''}
      </div>
      ${task.description ? html`<div class="task-desc">${unsafeHTML(marked.parse(task.description))}</div>` : ''}

      <div class="card" style="margin-bottom:var(--space-md);display:flex;align-items:center;gap:0.75rem">
        <span style="font-size:0.85rem"><strong>Trigger:</strong> ${task.trigger_label || 'manual'}</span>
        ${task.daemon_state ? this._renderDaemonState(task.daemon_state) : ''}
        <button class="btn btn-sm" style="background:var(--muted);margin-left:auto"
          @click=${() => { this._triggerOpen = !this._triggerOpen; }}>&#9998; Edit trigger</button>
      </div>

      ${isPipeline ? this._renderStages(task) : ''}

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

      ${this._editorOpen && hasEditor ? html`
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

      <div style="display:flex;align-items:baseline;gap:var(--space-md);margin-top:var(--space-lg)">
        <h2 style="margin:0">Recent runs</h2>
        <label class="meta" style="margin-left:auto;cursor:pointer">
          <input type="checkbox"
            .checked=${this._showStages}
            @change=${e => { this._showStages = e.target.checked; }}>
          Show stages
        </label>
      </div>
      <table>
        <thead><tr><th>Run ID</th><th>Status</th><th>Started</th><th>Duration</th><th></th></tr></thead>
        <tbody>
          ${!this._runs?.length ? html`
            <tr><td colspan="5" style="text-align:center;color:var(--muted)">No runs yet.</td></tr>
          ` : this._buildRunItems().map(item => item.kind === 'group'
              ? this._renderGroupRow(item)
              : this._renderRunRow(item.run))}
        </tbody>
      </table>`;
  }
}

customElements.define('dc-task-detail', DcTaskDetail);
