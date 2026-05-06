import { LitElement, html } from 'https://esm.sh/lit@3';
import { get, post } from '../lib/api.js';
import { wsOn } from '../lib/ws.js';
import { navigate } from '../lib/router.js';
import { relayHookBaseURL, webhookURL } from '../lib/config.js';

class DcTaskList extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _tasks:         { state: true },
    _sources:       { state: true },
    _error:         { state: true },
    _filter:        { state: true },
    _collapsed:     { state: true },
    _togglePending: { state: true },
  };

  constructor() {
    super();
    this._tasks = null;
    this._sources = new Map(); // source name → entry (with pull-health fields)
    this._error = null;
    this._filter = '';
    // Persisted collapse state: set of namespace keys that are folded shut.
    this._collapsed = this._loadCollapsed();
    this._relayBase = '';
    this._offFinished = null;
    this._offStarted = null;
    this._offChanged = null;
    this._togglePending = new Set();
  }

  _loadCollapsed() {
    try {
      const raw = localStorage.getItem('dicode.task-list.collapsed');
      return new Set(raw ? JSON.parse(raw) : []);
    } catch {
      return new Set();
    }
  }

  _saveCollapsed() {
    try {
      localStorage.setItem('dicode.task-list.collapsed', JSON.stringify([...this._collapsed]));
    } catch { /* quota — ignore */ }
  }

  _toggleGroup(ns) {
    const next = new Set(this._collapsed);
    if (next.has(ns)) next.delete(ns); else next.add(ns);
    this._collapsed = next;
    this._saveCollapsed();
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
      const [tasks, sources, base] = await Promise.all([
        get('/api/tasks'),
        get('/api/sources').catch(() => []), // sources endpoint is optional; don't fail the page
        relayHookBaseURL(),
      ]);
      this._tasks = tasks;
      this._sources = new Map((sources || []).map(s => [s.name, s]));
      this._relayBase = base;
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

  // Group tasks by top-level namespace segment, applying the active
  // filter as a case-insensitive substring match against ID, name, and
  // trigger label. Tasks without '/' in their ID go in the '' bucket.
  // Empty groups are omitted from the result.
  _grouped() {
    const q = this._filter.trim().toLowerCase();
    const match = (t) => {
      if (!q) return true;
      return (
        t.id.toLowerCase().includes(q) ||
        (t.name || '').toLowerCase().includes(q) ||
        (t.trigger_label || '').toLowerCase().includes(q)
      );
    };

    const map = new Map();
    for (const t of this._tasks) {
      if (!match(t)) continue;
      const ns = t.id.includes('/') ? t.id.split('/')[0] : '';
      if (!map.has(ns)) map.set(ns, []);
      map.get(ns).push(t);
    }
    return [...map.entries()].sort(([a], [b]) => {
      if (a === '') return -1;
      if (b === '') return 1;
      return a.localeCompare(b);
    });
  }

  _matchingCount() {
    return this._grouped().reduce((n, [, ts]) => n + ts.length, 0);
  }

  // _pullDot renders a small colored dot in a source-group header
  // reflecting the last pull outcome.
  //   green = last pull OK
  //   red   = last pull failed (tooltip shows the error)
  //   grey  = no pull attempted yet OR never seen by /api/sources
  // Local sources (type: local) have nothing to pull, so they skip
  // the dot entirely.
  _pullDot(ns) {
    const src = this._sources.get(ns);
    if (src && src.type === 'local') return '';

    let color = '#8c96a3'; // grey default: unknown / pending
    let tip = 'no pull data yet';

    if (src && src.last_pull_at) {
      const when = new Date(src.last_pull_at).toLocaleString();
      if (src.last_pull_ok) {
        color = '#3fb950';
        tip = `last pull: ${when} · OK`;
      } else {
        color = '#f85149';
        tip = `last pull: ${when} · ${src.last_pull_error || 'error'}`;
      }
    } else if (!src) {
      tip = 'source not registered with /api/sources';
    }

    return html`<span
      title=${tip}
      style="display:inline-block;width:0.55rem;height:0.55rem;border-radius:50%;background:${color};cursor:help"></span>`;
  }

  // displayID strips the namespace prefix (e.g. "examples/hello-cron"
  // → "hello-cron") so the task name isn't redundant with the source
  // group heading. The link's href still uses the full id.
  _displayID(id, ns) {
    if (ns && id.startsWith(ns + '/')) return id.slice(ns.length + 1);
    return id;
  }

  _taskRow(t, ns) {
    const shown = this._displayID(t.id, ns);
    const id = t.id;
    const disabled = t.enabled === false;
    const pending = this._togglePending.has(id);
    return html`
      <tr class=${disabled ? 'disabled' : ''} data-task-id=${id}>
        <td><a href="/tasks/${t.id}" @click=${e => { e.preventDefault(); navigate('/tasks/' + t.id); }}>${shown}</a>${disabled ? html`<span class="badge-paused">paused</span>` : ''}</td>
        <td>${t.name}</td>
        <td>${t.trigger?.Webhook
          ? html`<a href="${webhookURL(this._relayBase, t.trigger.Webhook)}" target="_blank" class="meta">${t.trigger_label}</a>`
          : html`<span class="meta">${t.trigger_label || 'manual'}</span>`}</td>
        <td>${t.last_run_id
          ? html`<a href="/runs/${t.last_run_id}" @click=${e => { e.preventDefault(); navigate('/runs/' + t.last_run_id); }}>${t.last_run_id.slice(0, 8)}</a>`
          : '—'}</td>
        <td>${t.last_run_status
          ? html`<span class="badge badge-${t.last_run_status}">${t.last_run_status}</span>`
          : '—'}</td>
        <td style="white-space:nowrap">
          <button class=${`toggle-btn ${disabled ? 'off' : 'on'}`}
                  title=${disabled ? 'Enable task' : 'Disable task'}
                  ?disabled=${pending}
                  @click=${(e) => this._onToggle(e, t)}>
            ${disabled
              ? html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <circle cx="12" cy="12" r="9"/>
                  <line x1="6" y1="6" x2="18" y2="18"/>
                </svg>`
              : html`<svg viewBox="0 0 24 24" fill="currentColor"><circle cx="12" cy="12" r="9"/></svg>`}
          </button>
          <button class="btn btn-sm" @click=${() => this._run(t.id)}>&#9654; Run</button>
        </td>
      </tr>`;
  }

  // _pullSummary renders the inline "last pull …" text next to the
  // source-group count. Nothing for local sources.
  _pullSummary(ns) {
    const src = this._sources.get(ns);
    if (!src || src.type === 'local') return '';
    if (!src.last_pull_at) {
      return html`<span class="meta">· last pull: —</span>`;
    }
    const rel = this._relTime(src.last_pull_at);
    if (src.last_pull_ok) {
      return html`<span class="meta">· last pull: ${rel}</span>`;
    }
    return html`<span class="meta" style="color:#f85149"
      title=${src.last_pull_error || 'error'}>· last pull: ${rel} · failed</span>`;
  }

  _relTime(iso) {
    const t = new Date(iso).getTime();
    if (!t) return '—';
    const s = Math.max(0, Math.round((Date.now() - t) / 1000));
    if (s < 30) return 'just now';
    if (s < 90) return '1m ago';
    if (s < 3600) return `${Math.round(s / 60)}m ago`;
    if (s < 5400) return '1h ago';
    if (s < 86400) return `${Math.round(s / 3600)}h ago`;
    return `${Math.round(s / 86400)}d ago`;
  }

  async _onToggle(e, task) {
    e.stopPropagation();
    const id = task.id;
    if (!id) return;
    const next = !(task.enabled === false ? false : true);

    this._togglePending = new Set([...this._togglePending, id]);
    task.enabled = next;
    this.requestUpdate();

    try {
      const res = await fetch(`/api/tasks/${id}/overrides`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: next }),
      });
      if (res.status === 409) {
        this._toast('Config changed externally — reloading');
        await this._load();
        return;
      }
      if (res.status === 422) {
        const body = await res.json().catch(() => ({}));
        this._toast(body.error || 'Cannot toggle: ancestor source is disabled');
        task.enabled = !next;
        return;
      }
      if (!res.ok) {
        const body = await res.text();
        this._toast(`Toggle failed: ${body || res.statusText}`);
        task.enabled = !next;
        return;
      }
      // 200 — keep optimistic state, _load polling will reconcile.
    } catch (err) {
      this._toast(`Toggle failed: ${err.message}`);
      task.enabled = !next;
    } finally {
      const next2 = new Set(this._togglePending);
      next2.delete(id);
      this._togglePending = next2;
      this.requestUpdate();
    }
  }

  _toast(msg) {
    window.dispatchEvent(new CustomEvent('dc-toast', { detail: { message: msg, kind: 'error' } }));
    console.warn('[task-toggle]', msg);
  }

  render() {
    if (this._error) return html`<p style="color:red">Error: ${this._error}</p>`;
    if (!this._tasks) return html`<div class="meta">Loading…</div>`;

    const groups = this._grouped();
    const isNamespaced = groups.some(([ns]) => ns !== '');

    const matching = this._matchingCount();
    const noMatches = this._filter && matching === 0;

    return html`
      <style>
        dc-task-list tr.disabled { opacity: 0.55; }
        dc-task-list tr.disabled td { font-style: italic; }
        dc-task-list .badge-paused {
          display: inline-block;
          margin-left: 0.5rem;
          padding: 0 0.4rem;
          font-size: 0.7rem;
          border-radius: 3px;
          background: var(--badge-bg, #2a2a2a);
          color: var(--badge-fg, #aaa);
          vertical-align: middle;
        }
        dc-task-list .toggle-btn {
          background: none;
          border: none;
          cursor: pointer;
          padding: 0.25rem;
          color: var(--muted);
          vertical-align: middle;
        }
        dc-task-list .toggle-btn:disabled { cursor: wait; }
        dc-task-list .toggle-btn.on  { color: var(--accent, #4caf50); }
        dc-task-list .toggle-btn.off { color: var(--muted, #888); }
        dc-task-list .toggle-btn svg { display: inline-block; width: 18px; height: 18px; vertical-align: middle; }
      </style>
      <div style="display:flex;align-items:center;gap:var(--space-md);margin-bottom:var(--space-md)">
        <h1 style="margin:0">Tasks</h1>
        <span class="meta" style="flex:0 0 auto">
          ${this._filter ? `${matching} / ${this._tasks.length}` : ''}
        </span>
        <div style="flex:1"></div>
        <input type="search"
          placeholder="Filter by id, name, or trigger…"
          .value=${this._filter}
          @input=${e => this._filter = e.target.value}
          style="min-width:14rem;padding:0.35rem 0.6rem;border-radius:6px;
                 border:1px solid var(--border);background:var(--bg-alt);
                 color:inherit;font:inherit" />
      </div>
      ${this._tasks.length === 0 ? html`
        <div class="card" style="text-align:center;color:var(--muted);padding:var(--space-xl)">
          No tasks found. Add tasks to your data directory.
        </div>
      ` : noMatches ? html`
        <div class="card" style="text-align:center;color:var(--muted);padding:var(--space-xl)">
          No tasks match “${this._filter}”.
        </div>
      ` : isNamespaced ? html`
        ${groups.map(([ns, tasks]) => {
          const collapsed = ns && this._collapsed.has(ns);
          return html`
          <div style="margin-bottom:1.25rem">
            ${ns ? html`
              <div role="button" tabindex="0"
                aria-expanded=${collapsed ? 'false' : 'true'}
                @click=${() => this._toggleGroup(ns)}
                @keydown=${e => {
                  if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); this._toggleGroup(ns); }
                }}
                style="display:flex;align-items:center;gap:var(--space-sm);margin-bottom:0.4rem;cursor:pointer;user-select:none"
                title=${collapsed ? 'Expand group' : 'Collapse group'}>
                <span style="display:inline-block;width:0.8rem;text-align:center;color:var(--muted);transition:transform 0.1s ease;transform:rotate(${collapsed ? '-90' : '0'}deg)">▾</span>
                ${this._pullDot(ns)}
                <span style="font-size:0.78rem;font-weight:700;color:var(--lavender);text-transform:uppercase;letter-spacing:0.05em">${ns}</span>
                <span class="meta">(${tasks.length})</span>
                ${this._pullSummary(ns)}
              </div>` : ''}
            ${collapsed ? '' : html`
              <table>
                <thead><tr><th>ID</th><th>Name</th><th>Trigger</th><th>Last Run</th><th>Status</th><th></th></tr></thead>
                <tbody>${tasks.map(t => this._taskRow(t, ns))}</tbody>
              </table>`}
          </div>
        `;})}
      ` : html`
        <table>
          <thead><tr><th>ID</th><th>Name</th><th>Trigger</th><th>Last Run</th><th>Status</th><th></th></tr></thead>
          <tbody>${this._tasks.map(t => this._taskRow(t, ''))}</tbody>
        </table>
      `}`;
  }
}

customElements.define('dc-task-list', DcTaskList);
