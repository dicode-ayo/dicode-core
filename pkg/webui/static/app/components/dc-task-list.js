import { LitElement, html } from 'https://esm.sh/lit@3';
import { get, post } from '../lib/api.js';
import { wsOn } from '../lib/ws.js';
import { navigate } from '../lib/router.js';

class DcTaskList extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _tasks:  { state: true },
    _error:  { state: true },
  };

  constructor() {
    super();
    this._tasks = null;
    this._error = null;
    this._offFinished = null;
    this._offStarted = null;
    this._offChanged = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._load();
    this._offFinished = wsOn('run:finished', d => {
      if (!this._tasks) return;
      this._tasks = this._tasks.map(t =>
        t.id === d.taskID ? { ...t, last_run_id: d.runID, last_run_status: d.status } : t
      );
    });
    this._offStarted = wsOn('run:started', d => {
      if (!this._tasks) return;
      this._tasks = this._tasks.map(t =>
        t.id === d.taskID ? { ...t, last_run_id: d.runID, last_run_status: 'running' } : t
      );
    });
    this._offChanged = wsOn('tasks:changed', () => this._load());
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this._offFinished?.();
    this._offStarted?.();
    this._offChanged?.();
  }

  async _load() {
    try {
      this._tasks = await get('/api/tasks');
    } catch(e) {
      this._error = e.message;
    }
  }

  async _run(taskID) {
    try {
      const r = await post(`/api/tasks/${encodeURIComponent(taskID)}/run`);
      navigate(`/runs/${r.runId}`);
    } catch(e) { alert('Failed to run task: ' + e.message); }
  }

  // Group tasks by top-level namespace segment.
  // Tasks without '/' in their ID go in the '' (ungrouped) bucket.
  _grouped() {
    const map = new Map();
    for (const t of this._tasks) {
      const ns = t.id.includes('/') ? t.id.split('/')[0] : '';
      if (!map.has(ns)) map.set(ns, []);
      map.get(ns).push(t);
    }
    // Sort: ungrouped first, then namespaces alphabetically
    return [...map.entries()].sort(([a], [b]) => {
      if (a === '') return -1;
      if (b === '') return 1;
      return a.localeCompare(b);
    });
  }

  _taskRow(t) {
    return html`
      <tr>
        <td><a href="/tasks/${t.id}" @click=${e => { e.preventDefault(); navigate('/tasks/' + t.id); }}>${t.id}</a></td>
        <td>${t.name}</td>
        <td>${t.trigger?.Webhook
          ? html`<a href="${t.trigger.Webhook}" target="_blank" class="meta">${t.trigger_label}</a>`
          : html`<span class="meta">${t.trigger_label || 'manual'}</span>`}</td>
        <td>${t.last_run_id
          ? html`<a href="/runs/${t.last_run_id}" @click=${e => { e.preventDefault(); navigate('/runs/' + t.last_run_id); }}>${t.last_run_id.slice(0, 8)}</a>`
          : '—'}</td>
        <td>${t.last_run_status
          ? html`<span class="badge badge-${t.last_run_status}">${t.last_run_status}</span>`
          : '—'}</td>
        <td><button class="btn btn-sm" @click=${() => this._run(t.id)}>&#9654; Run</button></td>
      </tr>`;
  }

  render() {
    if (this._error) return html`<p style="color:red">Error: ${this._error}</p>`;
    if (!this._tasks) return html`<div class="meta">Loading…</div>`;

    const groups = this._grouped();
    const isNamespaced = groups.some(([ns]) => ns !== '');

    return html`
      <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem">
        <h1 style="margin:0">Tasks</h1>
      </div>
      ${this._tasks.length === 0 ? html`
        <div class="card" style="text-align:center;color:#888;padding:2rem">
          No tasks found. Add tasks to your data directory.
        </div>
      ` : isNamespaced ? html`
        ${groups.map(([ns, tasks]) => html`
          <div style="margin-bottom:1.25rem">
            ${ns ? html`
              <div style="display:flex;align-items:center;gap:0.5rem;margin-bottom:0.4rem">
                <span style="font-size:0.78rem;font-weight:700;color:#7c3aed;text-transform:uppercase;letter-spacing:0.05em">${ns}</span>
                <span class="meta">(${tasks.length})</span>
              </div>` : ''}
            <table>
              <thead><tr><th>ID</th><th>Name</th><th>Trigger</th><th>Last Run</th><th>Status</th><th></th></tr></thead>
              <tbody>${tasks.map(t => this._taskRow(t))}</tbody>
            </table>
          </div>
        `)}
      ` : html`
        <table>
          <thead><tr><th>ID</th><th>Name</th><th>Trigger</th><th>Last Run</th><th>Status</th><th></th></tr></thead>
          <tbody>${this._tasks.map(t => this._taskRow(t))}</tbody>
        </table>
      `}`;
  }
}

customElements.define('dc-task-list', DcTaskList);
