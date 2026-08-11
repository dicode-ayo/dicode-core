import { html } from 'https://esm.sh/lit@3';
import { ref } from 'https://esm.sh/lit@3/directives/ref.js';
import { DcElement } from '../lib/dc-element.js';
import './dc-card.js';
import './dc-page-header.js';
import './dc-empty-state.js';
import './dc-icon-button.js';
import './dc-status-badge.js';
import './dc-table.js';

// <dc-ui-kit-demo> — Stage 1 (#93) proof page: exercises all six new
// primitives (dc-card, dc-page-header, dc-empty-state, dc-icon-button,
// dc-status-badge, dc-table) plus the Stage 2 DcElement base class, so the
// primitives can be reviewed/regression-tested in isolation before any
// existing component adopts them (Stage 3+, out of scope here).
//
// Routed at /ui-kit by app.js. Not linked from the persistent nav — reached
// by direct URL, same as any other route.
//
// This component itself is a light-DOM "app component" (like dc-task-list,
// etc.) that *composes* the Shadow-DOM primitives — see
// docs/concepts/webui-components.md for the split.
const SVG_TRASH = html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
  <polyline points="3 6 5 6 21 6"/>
  <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>
</svg>`;

const SVG_REFRESH = html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
  <polyline points="23 4 23 10 17 10"/>
  <path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"/>
</svg>`;

const STATUS_VALUES = [
  'success', 'failure', 'running', 'crashlooping',
  'cancelled', 'manual', 'suspended', 'resumed', 'unknown-status',
];

const DEMO_TABLE_HEAD = ['ID', 'Name', 'Status'];
const DEMO_TABLE_ROWS = [
  { id: 'task-1', name: 'Example One', status: 'success' },
  { id: 'task-2', name: 'Example Two', status: 'running' },
  { id: 'task-3', name: 'Example Three', status: 'failure' },
];

class DcUiKitDemo extends DcElement {
  static properties = {
    ...DcElement.properties,
    _demoResult: { state: true },
  };

  constructor() {
    super();
    this._demoResult = null;
  }

  // Exercises DcElement's _fetch() helper with a fake async call so
  // _loading/_error are visibly demonstrated without a real network
  // dependency (keeps this page deterministic for e2e/manual QA).
  async _simulate(shouldFail) {
    this._demoResult = null;
    const result = await this._fetch(() => new Promise((resolve, reject) => {
      setTimeout(() => {
        if (shouldFail) reject(new Error('simulated failure'));
        else resolve('simulated data loaded OK');
      }, 300);
    }));
    if (result) this._demoResult = result;
  }

  // Populates <dc-table id="table-with-rows">'s light-DOM row children via
  // real DOM APIs (document.createElement + appendChild) rather than as
  // literal <tr>/<th>/<td> markup in the `html` template below.
  //
  // Markup would silently fail: the HTML parser only creates <tr>/<td>/<th>
  // elements while in one of its table-specific insertion modes, which it
  // only enters after already having opened a literal <table> tag in the
  // SAME parse. <dc-table> is not a <table> — so parsing a template
  // containing `<dc-table><tr>...</tr></dc-table>` leaves the parser in its
  // default "in body" mode the whole way through, where a <tr>/<td>/<th>
  // start tag is a parse error and the tag is dropped outright (its text
  // content survives as loose text, but no element is created — verified
  // empirically, not just from the spec text). This is a general Lit/HTML
  // constraint, not a dc-table-specific one: it applies to *any* element
  // that isn't literally <table>, so any consumer wanting rows built from
  // markup runs into it too. document.createElement bypasses HTML parsing
  // entirely, so it's unaffected.
  //
  // Guarded so the `ref` directive's callback (which only fires again if
  // this exact element instance is torn down and recreated, not on every
  // re-render — but stay defensive) never double-populates.
  _populateTableRows(el) {
    if (!el || el.childElementCount > 0) return;

    const head = document.createElement('tr');
    head.slot = 'head';
    for (const label of DEMO_TABLE_HEAD) {
      const th = document.createElement('th');
      th.textContent = label;
      head.appendChild(th);
    }
    el.appendChild(head);

    for (const row of DEMO_TABLE_ROWS) {
      const tr = document.createElement('tr');
      const tdID = document.createElement('td');
      tdID.textContent = row.id;
      const tdName = document.createElement('td');
      tdName.textContent = row.name;
      const tdStatus = document.createElement('td');
      const badge = document.createElement('dc-status-badge');
      badge.status = row.status;
      tdStatus.appendChild(badge);
      tr.append(tdID, tdName, tdStatus);
      el.appendChild(tr);
    }
  }

  render() {
    return html`
      <dc-page-header heading="UI Kit" subtitle="Stage 1 primitives (#93) — dc-card, dc-page-header, dc-empty-state, dc-icon-button, dc-status-badge, dc-table">
        <button class="btn secondary" @click=${() => location.reload()}>Reload</button>
      </dc-page-header>

      <dc-card heading="dc-status-badge">
        <p class="meta" style="margin-bottom:var(--space-sm)">One pill per known status value, plus an unrecognized value falling back to the neutral style.</p>
        <div style="display:flex;gap:var(--space-sm);flex-wrap:wrap" id="status-badges">
          ${STATUS_VALUES.map(s => html`<dc-status-badge status=${s}></dc-status-badge>`)}
        </div>
      </dc-card>

      <dc-card heading="dc-icon-button">
        <div style="display:flex;gap:var(--space-sm);align-items:center">
          <dc-icon-button label="Refresh" id="icon-btn-default">${SVG_REFRESH}</dc-icon-button>
          <dc-icon-button label="Delete" variant="danger" id="icon-btn-danger">${SVG_TRASH}</dc-icon-button>
          <dc-icon-button label="Disabled example" disabled id="icon-btn-disabled">${SVG_TRASH}</dc-icon-button>
        </div>
      </dc-card>

      <dc-card heading="dc-card">
        <dc-card id="card-slots-demo">
          <div slot="title"><h2>Custom title slot</h2></div>
          <div slot="actions">
            <button class="btn btn-sm secondary">Action</button>
          </div>
          <p>This card demonstrates the <code>title</code> and <code>actions</code> slots alongside default body content.</p>
        </dc-card>
      </dc-card>

      <dc-card heading="dc-empty-state">
        <dc-empty-state id="empty-state-demo" icon="📭" message="No notifications yet.">
          <button class="btn btn-sm">Create one</button>
        </dc-empty-state>
      </dc-card>

      <dc-card heading="dc-table — with rows">
        <!-- Rows are populated imperatively via ${ref(...)} — see
             _populateTableRows's doc comment for why they can't be written
             as literal <tr> markup here. -->
        <dc-table id="table-with-rows" ${ref(el => this._populateTableRows(el))}></dc-table>
      </dc-card>

      <dc-card heading="dc-table — empty state">
        <!-- emptyMessage/emptyIcon are set as JS properties (.prop=) rather
             than attributes — Lit's default attribute reflection lowercases
             property names verbatim (no kebab-case insertion), so an
             "empty-message" attribute would not map to emptyMessage. -->
        <dc-table id="table-empty" .emptyMessage=${'No rows slotted.'} .emptyIcon=${'🗂️'}></dc-table>
      </dc-card>

      <dc-card heading="DcElement base class (Stage 2)">
        <p class="meta" style="margin-bottom:var(--space-sm)">Demonstrates the shared <code>_loading</code>/<code>_error</code> state and <code>_fetch()</code> helper with a simulated async call.</p>
        <div style="display:flex;gap:var(--space-sm);margin-bottom:var(--space-sm)">
          <button class="btn btn-sm" id="simulate-ok" @click=${() => this._simulate(false)} ?disabled=${this._loading}>Simulate success</button>
          <button class="btn btn-sm danger" id="simulate-fail" @click=${() => this._simulate(true)} ?disabled=${this._loading}>Simulate failure</button>
        </div>
        ${this._loading ? html`<div class="meta" id="demo-loading">Loading…</div>` : ''}
        ${this._error ? html`<p style="color:var(--red)" id="demo-error">Error: ${this._error}</p>` : ''}
        ${this._demoResult ? html`<p class="meta" id="demo-result">${this._demoResult}</p>` : ''}
      </dc-card>
    `;
  }
}

customElements.define('dc-ui-kit-demo', DcUiKitDemo);
