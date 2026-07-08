import { LitElement, html } from 'https://esm.sh/lit@3';
import { unsafeHTML } from 'https://esm.sh/lit@3/directives/unsafe-html.js';
import { get, post } from '../lib/api.js';
import { wsOn } from '../lib/ws.js';
import { navigate } from '../lib/router.js';
import { fmtTime, fmtDuration } from '../lib/utils.js';
import { ansiToHtml } from '../lib/ansi.js';

class DcRunDetail extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    runid:     { type: String },
    _run:      { state: true },
    _logs:     { state: true },
    _error:    { state: true },
    _status:   { state: true },
    _duration: { state: true },
    _parent:   { state: true }, // Run record for ParentRunID, or null (#115)
    _children: { state: true }, // child Run[] for the Sub-runs panel (#115)
    _resuming:     { state: true }, // resume submit in flight (#95)
    _resumeError:  { state: true }, // last resume submit error, or null (#95)
  };

  constructor() {
    super();
    this._run = null; this._logs = []; this._error = null;
    this._status = null; this._duration = null;
    this._parent = null; this._children = null;
    this._resuming = false; this._resumeError = null;
    this._offLog = null; this._offFinished = null;
  }

  updated(changed) {
    if (changed.has('runid') && changed.get('runid') !== undefined) {
      this._cleanup();
      this._load();
    }
  }

  connectedCallback() {
    super.connectedCallback();
    this._load();
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this._cleanup();
  }

  _cleanup() {
    this._offLog?.(); this._offLog = null;
    this._offFinished?.(); this._offFinished = null;
  }

  async _load() {
    if (!this.runid) return;
    this._run = null; this._logs = []; this._error = null;
    this._status = null; this._duration = null;
    this._parent = null; this._children = null;
    this._resuming = false; this._resumeError = null;
    try {
      // Children fetch is fire-and-forget — failure here shouldn't block the
      // main run/logs load (Sub-runs panel just stays empty).
      const childrenP = get(`/api/runs?parent=${encodeURIComponent(this.runid)}&limit=50`)
        .catch(() => []);
      const [run, logs] = await Promise.all([
        get(`/api/runs/${this.runid}`),
        get(`/api/runs/${this.runid}/logs`),
      ]);
      this._run = run;
      this._logs = (logs || []).map(l => ({
        id: l.id,
        level: l.level,
        message: l.message,
        time: fmtTime(l.ts),
      }));
      this._status = run.status || run.Status;
      this._children = await childrenP;

      // #115: if this run has a parent, fetch the parent's record too so the
      // back-link can show the parent task name instead of just an ID prefix.
      const parentID = run.ParentRunID || run.parent_run_id;
      if (parentID) {
        try { this._parent = await get(`/api/runs/${encodeURIComponent(parentID)}`); }
        catch (_) { this._parent = { ID: parentID }; }
      }

      if (this._status === 'running') this._wireWS();

      await this.updateComplete;
      const logEl = this.querySelector('#log-output');
      if (logEl) logEl.scrollTop = logEl.scrollHeight;
    } catch(e) {
      this._error = e.message;
    }
  }

  _wireWS() {
    this._offLog = wsOn('run:log', d => {
      if (d.runID !== this.runid) return;
      this._logs = [...this._logs, {
        id: Date.now(),
        level: d.level,
        message: d.message,
        time: new Date(d.ts < 1e11 ? d.ts * 1000 : d.ts).toLocaleTimeString(),
      }];
      this.updateComplete.then(() => {
        const el = this.querySelector('#log-output');
        if (el) el.scrollTop = el.scrollHeight;
      });
    });

    this._offFinished = wsOn('run:finished', d => {
      if (d.runID !== this.runid) return;
      this._offLog?.(); this._offLog = null;
      this._offFinished?.(); this._offFinished = null;
      this._status = d.status;
      this._duration = (d.durationMs / 1000).toFixed(1) + 's';
      if (d.outputContentType) {
        setTimeout(() => navigate(`/runs/${this.runid}`, false), 200);
      }
    });
  }

  async _kill() {
    if (!confirm('Kill this run?')) return;
    try { await post(`/api/runs/${this.runid}/kill`); } catch(e) { alert('Kill failed: ' + e.message); }
  }

  async _replay() {
    try {
      const res = await post(`/api/runs/${this.runid}/replay`, {});
      const newID = res?.run_id;
      if (newID) navigate(`/runs/${newID}`);
    } catch(e) {
      alert('Replay failed: ' + e.message);
    }
  }

  // #512: is this JSON-Schema property required? Derived from the schema's
  // top-level `required` array.
  _isRequired(name) {
    const req = this._run?.resume_schema?.required;
    return Array.isArray(req) && req.includes(name);
  }

  // #512: render a textarea for a string property when the schema hints at
  // multi-line input (format:"textarea"/"multiline") — otherwise a text input.
  _isTextarea(prop) {
    return prop?.type === 'string' && (prop.format === 'textarea' || prop.format === 'multiline');
  }

  // #512: coerce a control's value to the property's declared JSON type so the
  // task receives a typed value (number field → number, boolean → bool). Empty
  // optional values return undefined so they are omitted from the submission.
  _coerceValue(prop, el) {
    if (prop.type === 'boolean') return el.checked;
    if (Array.isArray(prop.enum)) {
      if (el.value === '') return undefined;
      // The <option> value carries the enum index, preserving the entry's
      // original JSON type (a numeric enum stays numeric).
      const i = Number(el.value);
      return prop.enum[i];
    }
    if (el.value === '') return undefined;
    if (prop.type === 'number' || prop.type === 'integer') return Number(el.value);
    return el.value;
  }

  // #512: submit a suspended run's input. Values are collected from the
  // light-DOM form, coerced to the schema's declared types, and POSTed; the
  // server validates against the stored schema and resolves the resume token
  // itself. On success we navigate to the continuation run.
  async _submitResume(e) {
    e.preventDefault();
    const schema = this._run?.resume_schema;
    if (!schema) return;
    const props = schema.properties || {};
    const form = e.target;
    const values = {};
    for (const [name, prop] of Object.entries(props)) {
      const el = form.elements.namedItem(name);
      if (!el) continue;
      const v = this._coerceValue(prop, el);
      if (this._isRequired(name) && (v === '' || v === null || v === undefined)) {
        this._resumeError = `${prop.title || name} is required`;
        return;
      }
      // Omit empty optional values so the task sees them as absent.
      if (v === undefined) continue;
      values[name] = v;
    }
    this._resumeError = null;
    this._resuming = true;
    try {
      const res = await post(`/api/runs/${this.runid}/resume`, values);
      const newID = res?.run_id;
      if (newID) navigate(`/runs/${newID}`);
    } catch (err) {
      this._resumeError = err.message;
    } finally {
      this._resuming = false;
    }
  }

  _renderResumeField(name, prop) {
    const req = this._isRequired(name) ? html`<span style="color:var(--red)"> *</span>` : '';
    const label = prop.title || name;
    const desc = prop.description ? html`<span class="meta" style="display:block;font-weight:normal">${prop.description}</span>` : '';
    let input;
    if (prop.type === 'boolean') {
      return html`<div style="margin-bottom:var(--space-md)">
        <label><input type="checkbox" name=${name} ?checked=${prop.default === true}> ${label}${req}</label>
        ${desc}
      </div>`;
    } else if (Array.isArray(prop.enum)) {
      input = html`<select name=${name} style="width:100%">
        ${prop.enum.map((o, i) => html`<option value=${i} ?selected=${o === prop.default}>${String(o)}</option>`)}
      </select>`;
    } else if (this._isTextarea(prop)) {
      input = html`<textarea name=${name} rows="4" .value=${prop.default ?? ''} style="width:100%"></textarea>`;
    } else if (prop.type === 'number' || prop.type === 'integer') {
      input = html`<input type="number" name=${name} step=${prop.type === 'integer' ? '1' : 'any'} .value=${prop.default ?? ''} style="width:100%">`;
    } else {
      input = html`<input type="text" name=${name} .value=${prop.default ?? ''} style="width:100%">`;
    }
    return html`<div style="margin-bottom:var(--space-md)">
      <label style="display:block;font-weight:var(--font-semibold);margin-bottom:.25rem">${label}${req}</label>
      ${desc}
      ${input}
    </div>`;
  }

  _renderResumeForm() {
    const schema = this._run?.resume_schema;
    if (!schema) return '';
    const props = Object.entries(schema.properties || {});
    return html`
      <h2 style="margin-top:var(--space-lg)">${schema.title || 'Waiting on your input'}</h2>
      <div class="card">
        ${schema.description ? html`<p class="meta" style="margin-top:0">${schema.description}</p>` : ''}
        <form @submit=${e => this._submitResume(e)}>
          ${props.map(([name, prop]) => this._renderResumeField(name, prop))}
          ${this._resumeError ? html`<p style="color:var(--red)">${this._resumeError}</p>` : ''}
          <div style="margin-top:var(--space-md)">
            <button class="btn" type="submit" ?disabled=${this._resuming}>${this._resuming ? 'Resuming…' : 'Submit'}</button>
          </div>
        </form>
      </div>`;
  }

  render() {
    if (this._error) return html`<p style="color:red">Error: ${this._error}</p>`;
    if (!this._run) return html`<div class="meta">Loading…</div>`;

    const run = this._run;
    const taskName     = run.task_name || run.task_id;
    const taskID       = run.task_id;
    const status       = this._status || run.status;
    const isRunning    = status === 'running';
    const startedAt    = run.started_at || run.StartedAt;
    const finishedAt   = run.finished_at || run.FinishedAt;
    const trigSrc      = run.trigger_source;
    const otype        = run.output_content_type;
    const ocontent     = run.output_content;
    const retval       = run.return_value;

    let displayRV = retval;
    if (retval) { try { displayRV = JSON.stringify(JSON.parse(retval), null, 2); } catch(_) {} }

    // #115: parent + children panels — only render when there's something
    // to show. The data is loaded once in _load(); empty arrays render
    // nothing so an isolated run looks identical to the pre-#115 view.
    const parent = this._parent;
    const parentTask = parent?.task_name || parent?.TaskName || parent?.task_id || parent?.TaskID;
    const children = this._children || [];

    return html`
      <div style="margin-bottom:var(--space-md)">
        <a href="tasks/${encodeURIComponent(taskID)}">← ${taskName}</a>
      </div>

      ${parent ? html`
        <div class="card" style="margin-bottom:var(--space-md);display:flex;gap:var(--space-md);align-items:center">
          <span class="meta">Parent run</span>
          <a href="/runs/${parent.ID || parent.id}">
            ${parentTask ? `${parentTask} · ` : ''}<code>${(parent.ID || parent.id || '').slice(0,8)}</code>
          </a>
        </div>` : ''}

      <div class="card">
        <div style="display:flex;gap:var(--space-md);align-items:center;flex-wrap:wrap">
          <span class="badge badge-${status}">${status}</span>
          <strong>${taskName}</strong>
          ${trigSrc ? html`<span class="meta badge badge-manual">${trigSrc}</span>` : ''}
          <span class="meta">Run <code>${this.runid.slice(0,8)}</code></span>
          <span class="meta">Started ${fmtTime(startedAt)}</span>
          <span class="meta">${this._duration || (finishedAt ? fmtDuration(startedAt, finishedAt) : isRunning ? 'running…' : '—')}</span>
          <a href="/runs/${this.runid}/result" target="_blank" class="btn btn-sm secondary" style="margin-left:auto">Result ↗</a>
          ${isRunning ? html`
            <button class="btn" style="background:var(--red)" @click=${() => this._kill()}>Kill</button>`
            : (status === 'success' || status === 'failure') ? html`
            <button class="btn btn-sm" @click=${() => this._replay()} title="Re-fire this run with its persisted input">Replay</button>` : ''}
        </div>
      </div>

      ${status === 'suspended' ? this._renderResumeForm() : ''}

      ${children.length ? html`
        <h2 style="margin-top:var(--space-lg)">Sub-runs <span class="meta">(${children.length})</span></h2>
        <table>
          <thead><tr><th>Run</th><th>Status</th><th>Started</th><th>Duration</th></tr></thead>
          <tbody>
            ${children.map(c => html`
              <tr>
                <td><a href="/runs/${c.ID}">${(c.TaskID || '?')} · <code>${c.ID.slice(0,8)}</code></a></td>
                <td><span class="badge badge-${c.Status}">${c.Status}</span></td>
                <td class="meta">${fmtTime(c.StartedAt)}</td>
                <td class="meta">${fmtDuration(c.StartedAt, c.FinishedAt)}</td>
              </tr>`)}
          </tbody>
        </table>` : ''}

      ${otype ? html`
        <div style="margin-bottom:.5rem">
          <h2 style="margin:0">Output</h2>
        </div>
        <div class="card" style="padding:0">
          ${otype === 'text/html'
            ? html`<iframe .srcdoc=${ocontent}
                sandbox="allow-scripts allow-same-origin"
                style="width:100%;border:none;border-radius:var(--radius-sm);display:block"
                @load=${e => { e.target.style.height = (e.target.contentDocument.body.scrollHeight + 32) + 'px'; }}>
              </iframe>`
            : html`<pre style="margin:0;border-radius:var(--radius-sm)">${ocontent}</pre>`}
        </div>` : retval ? html`
        <h2>Return value</h2>
        <div class="card" style="padding:0">
          <pre style="margin:0;border-radius:var(--radius-sm)">${displayRV}</pre>
        </div>` : ''}

      <h2>Logs</h2>
      <pre id="log-output" style="max-height:600px;overflow-y:auto">${this._logs.map(l => html`<span>[${l.level}] ${l.time} ${unsafeHTML(ansiToHtml(l.message))}\n</span>`)}</pre>`;
  }
}

customElements.define('dc-run-detail', DcRunDetail);
