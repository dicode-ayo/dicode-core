import { LitElement, html } from 'https://esm.sh/lit@3';
import { get, patch } from '../lib/api.js';

class DcSources extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _sources:   { state: true },
    _error:     { state: true },
    _status:    { state: true },   // map of name → status message
    _branches:  { state: true },   // map of name → branch list
    _devInputs: { state: true },   // map of name → local_path input value
  };

  constructor() {
    super();
    this._sources   = null;
    this._error     = null;
    this._status    = {};
    this._branches  = {};
    this._devInputs = {};
  }

  connectedCallback() {
    super.connectedCallback();
    this._load();
  }

  async _load() {
    try {
      this._sources = await get('/api/sources');
      this._error   = null;
    } catch(e) {
      this._error = e.message;
    }
  }

  async _loadBranches(name, url) {
    if (this._branches[name]) return; // already loaded
    try {
      const branches = await get(`/api/sources/${encodeURIComponent(name)}/branches`);
      this._branches = { ...this._branches, [name]: branches };
    } catch(e) {
      this._branches = { ...this._branches, [name]: [] };
    }
  }

  async _toggleDevMode(src) {
    const name    = src.name;
    const enabled = !src.dev_mode;
    const localPath = enabled ? (this._devInputs[name] || '') : '';

    if (enabled && !localPath) {
      this._setStatus(name, 'Enter a local path first.');
      return;
    }

    this._setStatus(name, 'Saving…');
    try {
      await patch(`/api/sources/${encodeURIComponent(name)}/dev`, {
        enabled,
        local_path: localPath,
      });
      this._setStatus(name, enabled ? 'Dev mode on ✓' : 'Dev mode off ✓');
      await this._load();
    } catch(e) {
      this._setStatus(name, 'Error: ' + e.message);
    }
  }

  _setStatus(name, msg) {
    this._status = { ...this._status, [name]: msg };
    if (msg && !msg.startsWith('Error')) {
      setTimeout(() => {
        this._status = { ...this._status, [name]: '' };
      }, 3000);
    }
  }

  _setDevInput(name, value) {
    this._devInputs = { ...this._devInputs, [name]: value };
  }

  render() {
    if (this._error) return html`<p style="color:red">Error: ${this._error}</p>`;

    return html`
      <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem">
        <h1 style="margin:0">Sources</h1>
        <button class="btn btn-sm secondary" @click=${() => this._load()}>&#8635; Reload</button>
      </div>

      ${!this._sources ? html`<div class="meta">Loading…</div>` : html`
        ${this._sources.length === 0 ? html`
          <div class="card" style="text-align:center;color:#888;padding:2rem">
            No sources configured. Add one in <a href="/config" @click=${e => { e.preventDefault(); window.navigate('/config'); }}>Config</a>.
          </div>
        ` : this._sources.map(src => this._sourceCard(src))}
      `}`;
  }

  _sourceCard(src) {
    const status    = this._status[src.name] || '';
    const devInput  = this._devInputs[src.name] || src.dev_path || '';
    const branches  = this._branches[src.name];
    const isTaskset = src.type === 'taskset';

    return html`
      <div class="card">
        <div style="display:flex;align-items:flex-start;gap:0.75rem;flex-wrap:wrap">
          <!-- Left: source info -->
          <div style="flex:1;min-width:0">
            <div style="display:flex;align-items:center;gap:0.5rem;margin-bottom:0.3rem">
              <strong>${src.name}</strong>
              <span class="badge badge-manual">${src.type}</span>
              ${src.dev_mode ? html`<span class="badge" style="background:#fef3c7;color:#92400e">DEV MODE</span>` : ''}
              ${status ? html`<span class="meta" style="${status.startsWith('Error') ? 'color:#842029' : ''}">${status}</span>` : ''}
            </div>
            ${src.url ? html`<div class="meta" style="word-break:break-all">${src.url}${src.branch ? html` &nbsp;<span style="color:#7c3aed">${src.branch}</span>` : ''}</div>` : ''}
            ${src.path && !src.url ? html`<div class="meta" style="word-break:break-all">${src.path}</div>` : ''}
            ${src.dev_mode && src.dev_path ? html`
              <div class="meta" style="margin-top:0.3rem;color:#92400e">
                &#128194; Dev path: <code>${src.dev_path}</code>
              </div>` : ''}
          </div>

          <!-- Right: branch picker (git sources) -->
          ${src.url && !isTaskset ? html`
            <div>
              <button class="btn btn-sm secondary" @click=${() => this._loadBranches(src.name, src.url)}>
                ${branches ? '&#8635;' : '&#9660;'} Branches
              </button>
              ${branches?.length ? html`
                <select style="margin-left:0.5rem;padding:0.25rem;border-radius:4px;border:1px solid #ccc;font-size:0.82rem">
                  ${branches.map(b => html`<option ?selected=${b === src.branch}>${b}</option>`)}
                </select>` : ''}
            </div>
          ` : ''}
        </div>

        <!-- Dev mode controls (taskset sources only) -->
        ${isTaskset ? html`
          <div style="margin-top:0.75rem;padding-top:0.75rem;border-top:1px solid #eee">
            <div style="display:flex;align-items:center;gap:0.75rem;flex-wrap:wrap">
              <span style="font-size:0.82rem;font-weight:600;color:#444">Dev mode</span>

              ${!src.dev_mode ? html`
                <input
                  .value=${devInput}
                  @input=${e => this._setDevInput(src.name, e.target.value)}
                  placeholder="/absolute/path/to/local/taskset.yaml"
                  style="flex:1;min-width:200px;padding:0.3rem 0.5rem;border:1px solid #ccc;border-radius:4px;font-size:0.82rem;font-family:monospace">
              ` : html`
                <code style="font-size:0.82rem;color:#92400e;flex:1">${src.dev_path}</code>
              `}

              <button
                class="btn btn-sm ${src.dev_mode ? '' : 'secondary'}"
                style="${src.dev_mode ? 'background:#f59e0b' : ''}"
                @click=${() => this._toggleDevMode(src)}>
                ${src.dev_mode ? '&#128990; Disable dev mode' : '&#128994; Enable dev mode'}
              </button>
            </div>
            ${!src.dev_mode ? html`
              <div class="meta" style="margin-top:0.4rem">
                Point to a local <code>taskset.yaml</code> to load tasks from your machine instead of the git ref.
                Dicode re-syncs immediately.
              </div>` : ''}
          </div>
        ` : ''}
      </div>`;
  }
}

customElements.define('dc-sources', DcSources);
