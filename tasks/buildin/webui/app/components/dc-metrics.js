import { LitElement, html } from 'https://esm.sh/lit@3';
import { get } from '../lib/api.js';

const POLL_INTERVAL_MS = 5000;

class DcMetrics extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _data:        { state: true },
    _error:       { state: true },
    _loading:     { state: true },
    _lastUpdated: { state: true },
  };

  constructor() {
    super();
    this._data        = null;
    this._error       = null;
    this._loading     = true;
    this._lastUpdated = null;
    this._timer       = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._poll();
    this._timer = setInterval(() => this._poll(), POLL_INTERVAL_MS);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    clearInterval(this._timer);
  }

  async _poll() {
    try {
      this._data        = await get('/api/metrics');
      this._error       = null;
      this._loading     = false;
      this._lastUpdated = new Date();
    } catch (e) {
      this._error   = e.message;
      this._loading = false;
    }
  }

  _fmtTime(d) {
    if (!d) return '';
    return d.toLocaleTimeString(undefined, {
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    });
  }

  _fmt(v, unit = '') {
    if (v === null || v === undefined) return '—';
    if (typeof v === 'number') return v.toFixed(1) + (unit ? '\u202f' + unit : '');
    return String(v);
  }

  _fmtMB(v)  { return this._fmt(v, 'MB'); }
  _fmtMs(v)  { return v != null ? this._fmt(v, 'ms') : '—'; }

  _row(label, value) {
    return html`
      <tr>
        <td class="meta" style="width:55%;padding-right:1rem">${label}</td>
        <td style="font-variant-numeric:tabular-nums">${value}</td>
      </tr>`;
  }

  render() {
    if (this._loading) return html`<div class="meta">Loading…</div>`;
    if (this._error)   return html`<p style="color:var(--red)">Error: ${this._error}</p>`;

    const { daemon, tasks } = this._data;

    return html`
      <div style="display:flex;align-items:center;gap:var(--space-md);margin-bottom:var(--space-md);flex-wrap:wrap">
        <h1 style="margin:0">Metrics</h1>
        <span class="meta">auto-refreshes every ${POLL_INTERVAL_MS / 1000}s</span>
        ${this._lastUpdated
          ? html`<span class="meta" style="margin-left:auto;font-variant-numeric:tabular-nums">
              updated ${this._fmtTime(this._lastUpdated)}
            </span>`
          : ''}
      </div>

      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:var(--space-md)">

        <!-- Tasks card -->
        <div class="card">
          <h3 style="margin:0 0 0.75rem">Tasks</h3>
          <table style="width:100%">
            <tbody>
              ${this._row('Active runs',    tasks.active_tasks)}
              ${tasks.max_concurrent_tasks > 0
                ? this._row(
                    'Concurrency slots',
                    `${tasks.active_task_slots} / ${tasks.max_concurrent_tasks}`,
                  )
                : this._row('Concurrency cap', 'unlimited')}
              ${tasks.waiting_tasks > 0
                ? this._row('Waiting for slot', tasks.waiting_tasks)
                : ''}
              ${tasks.children_rss_mb
                ? this._row('Child RSS (Deno)', this._fmtMB(tasks.children_rss_mb))
                : ''}
              ${tasks.children_cpu_ms != null
                ? this._row('Child CPU total', this._fmtMs(tasks.children_cpu_ms))
                : ''}
            </tbody>
          </table>
        </div>

        <!-- Daemon card -->
        <div class="card">
          <h3 style="margin:0 0 0.75rem">Daemon</h3>
          <table style="width:100%">
            <tbody>
              ${this._row('Goroutines',   daemon.goroutines)}
              ${this._row('Heap alloc',   this._fmtMB(daemon.heap_alloc_mb))}
              ${this._row('Heap sys',     this._fmtMB(daemon.heap_sys_mb))}
              ${daemon.cpu_ms != null
                ? this._row('Daemon CPU', this._fmtMs(daemon.cpu_ms))
                : ''}
            </tbody>
          </table>
        </div>

        <!-- DB card (optional, shown only when fields present) -->
        ${this._data.db ? html`
          <div class="card">
            <h3 style="margin:0 0 0.75rem">Database</h3>
            <table style="width:100%">
              <tbody>
                ${this._data.db.write_latency_p50_ms != null
                  ? this._row('Write p50', this._fmtMs(this._data.db.write_latency_p50_ms))
                  : ''}
                ${this._data.db.write_latency_p99_ms != null
                  ? this._row('Write p99', this._fmtMs(this._data.db.write_latency_p99_ms))
                  : ''}
                ${this._data.db.log_queue_depth != null
                  ? this._row('Log queue depth', this._data.db.log_queue_depth)
                  : ''}
              </tbody>
            </table>
          </div>
        ` : ''}
      </div>`;
  }
}

customElements.define('dc-metrics', DcMetrics);
