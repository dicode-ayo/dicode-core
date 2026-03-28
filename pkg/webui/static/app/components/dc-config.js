import { LitElement, html } from 'https://esm.sh/lit@3';
import { get, post, del } from '../lib/api.js';

class DcConfig extends LitElement {
  createRenderRoot() { return this; } // Monaco needs direct DOM access

  static properties = {
    _cfg:       { state: true },
    _raw:       { state: true },
    _error:     { state: true },
    _aiStatus:  { state: true },
    _srvStatus: { state: true },
    _srcStatus: { state: true },
    _cfgStatus: { state: true },
    _srcType:   { state: true },
  };

  constructor() {
    super();
    this._cfg = null; this._raw = null; this._error = null;
    this._aiStatus = ''; this._srvStatus = ''; this._srcStatus = ''; this._cfgStatus = '';
    this._srcType = 'local';
    this._editor = null;
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    if (this._editor) { this._editor.dispose(); this._editor = null; }
  }

  connectedCallback() {
    super.connectedCallback();
    this._load();
  }

  async _load() {
    try {
      const [cfg, raw] = await Promise.all([get('/api/config'), get('/api/config/raw')]);
      this._cfg = cfg; this._raw = raw;
    } catch(e) { this._error = e.message; return; }
    await this.updateComplete;
    this._initMonaco();
  }

  _initMonaco() {
    const container = this.querySelector('#config-monaco');
    if (!container) return;
    if (this._editor) { this._editor.dispose(); this._editor = null; }
    require.config({ paths: { vs: 'https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs' } });
    require(['vs/editor/editor.main'], () => {
      this._editor = monaco.editor.create(container, {
        value: this._raw?.content || '',
        language: 'yaml', theme: 'vs-dark', fontSize: 13, minimap: { enabled: false },
      });
    });
  }

  async _saveAI() {
    this._aiStatus = 'Saving…';
    try {
      await post('/api/settings/ai', {
        base_url:    this.querySelector('#ai-base-url')?.value,
        model:       this.querySelector('#ai-model')?.value,
        api_key_env: this.querySelector('#ai-key-env')?.value,
        api_key:     this.querySelector('#ai-key-val')?.value,
      });
      this._aiStatus = 'Saved ✓'; setTimeout(() => { this._aiStatus = ''; }, 2000);
    } catch(e) { this._aiStatus = 'Error: ' + e.message; }
  }

  async _saveServer() {
    this._srvStatus = 'Saving…';
    try {
      const tray = this.querySelector('#srv-tray')?.value === 'true';
      const secret = this.querySelector('#srv-secret')?.value;
      await post('/api/settings/server', {
        log_level: this.querySelector('#srv-log-level')?.value,
        tray,
        ...(secret ? { secret } : {}),
      });
      this._srvStatus = 'Saved ✓'; setTimeout(() => { this._srvStatus = ''; }, 2000);
    } catch(e) { this._srvStatus = 'Error: ' + e.message; }
  }

  async _removeSource(idx) {
    if (!confirm('Remove this source?')) return;
    try { await del(`/api/settings/sources/${idx}`); this._load(); }
    catch(e) { this._srcStatus = 'Error: ' + e.message; }
  }

  async _addSource() {
    const type = this._srcType;
    const body = { type };
    if (type === 'local') {
      body.path = this.querySelector('#new-src-path')?.value;
    } else {
      body.url       = this.querySelector('#new-src-url')?.value;
      body.branch    = this.querySelector('#new-src-branch')?.value || 'main';
      body.token_env = this.querySelector('#new-src-token-env')?.value;
    }
    try { await post('/api/settings/sources', body); this._load(); }
    catch(e) { this._srcStatus = 'Error: ' + e.message; }
  }

  async _saveRaw() {
    if (!this._editor) return;
    this._cfgStatus = 'Saving…';
    try {
      await post('/api/config/raw', { content: this._editor.getValue() });
      this._cfgStatus = 'Saved ✓'; setTimeout(() => { this._cfgStatus = ''; }, 2000);
    } catch(e) { this._cfgStatus = 'Error: ' + e.message; }
  }

  render() {
    if (this._error) return html`<p style="color:red">Error: ${this._error}</p>`;
    if (!this._cfg) return html`<div class="meta">Loading…</div>`;

    const ai      = this._cfg.AI      || this._cfg.ai      || {};
    const srv     = this._cfg.Server  || this._cfg.server  || {};
    const db      = this._cfg.Database|| this._cfg.database|| {};
    const sources = this._cfg.Sources || this._cfg.sources || [];
    const logLevel = this._cfg.LogLevel || this._cfg.log_level || 'info';
    const tray    = srv.Tray != null ? srv.Tray : (srv.tray != null ? srv.tray : true);

    return html`
      <h1>Configuration</h1>

      <!-- AI -->
      <div class="card">
        <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.75rem">
          <h2 style="margin:0">AI</h2>
          <span style="font-size:0.82rem;color:#888">${this._aiStatus}</span>
        </div>
        <div class="cfg-form">
          <div class="field"><label>Endpoint (base URL)</label>
            <input id="ai-base-url" .value=${ai.BaseURL || ai.base_url || ''} placeholder="leave blank for OpenAI">
            <div class="hint">OpenAI: leave blank &nbsp;|&nbsp; Claude: https://api.anthropic.com/v1 &nbsp;|&nbsp; Ollama: http://localhost:11434/v1</div>
          </div>
          <div class="field"><label>Model</label>
            <input id="ai-model" .value=${ai.Model || ai.model || ''} placeholder="gpt-4o">
          </div>
          <div class="field"><label>API key env var</label>
            <input id="ai-key-env" .value=${ai.APIKeyEnv || ai.api_key_env || ''} placeholder="OPENAI_API_KEY">
          </div>
          <div class="field"><label>API key (direct value)</label>
            <input id="ai-key-val" type="password" placeholder="paste to set; leave blank to keep current">
          </div>
          <button class="btn" @click=${() => this._saveAI()}>&#128190; Save AI settings</button>
        </div>
      </div>

      <!-- Server -->
      <div class="card">
        <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.75rem">
          <h2 style="margin:0">Server</h2>
          <span style="font-size:0.82rem;color:#888">${this._srvStatus}</span>
        </div>
        <div class="cfg-form">
          <div class="field"><label>Port</label>
            <input .value=${String(srv.Port || srv.port || '')} disabled style="color:#666;cursor:not-allowed">
            <div class="hint">Changing port requires restart; edit dicode.yaml directly.</div>
          </div>
          <div class="field"><label>Log level</label>
            <select id="srv-log-level">
              ${['debug','info','warn','error'].map(l => html`
                <option value=${l} ?selected=${logLevel === l}>${l}</option>`)}
            </select>
          </div>
          <div class="field"><label>System tray icon</label>
            <select id="srv-tray">
              <option value="true"  ?selected=${tray === true}>Enabled</option>
              <option value="false" ?selected=${tray === false}>Disabled</option>
            </select>
            <div class="hint">Takes effect on next restart.</div>
          </div>
          <div class="field"><label>Secrets passphrase</label>
            <input id="srv-secret" type="password" placeholder="leave blank to keep current">
          </div>
          <button class="btn" @click=${() => this._saveServer()}>&#128190; Save server settings</button>
        </div>
      </div>

      <!-- Database -->
      <div class="card">
        <h2>Database</h2>
        <table><tbody>
          <tr><th>Type</th><td>${db.Type || db.type || 'sqlite'}</td></tr>
          ${db.Path || db.path ? html`<tr><th>Path</th><td>${db.Path || db.path}</td></tr>` : ''}
          ${db.URLEnv || db.url_env ? html`<tr><th>URL env</th><td>${db.URLEnv || db.url_env}</td></tr>` : ''}
        </tbody></table>
      </div>

      <!-- Sources -->
      <div class="card">
        <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.75rem">
          <h2 style="margin:0">Sources (${sources.length})</h2>
          <span style="font-size:0.82rem;color:#888">${this._srcStatus}</span>
        </div>
        <table style="margin-bottom:1rem">
          <thead><tr><th>Type</th><th>Path / URL</th><th>Details</th><th></th></tr></thead>
          <tbody>
            ${sources.length === 0 ? html`
              <tr><td colspan="4" class="meta" style="text-align:center">No sources configured.</td></tr>
            ` : sources.map((s, i) => html`
              <tr>
                <td><span class="badge badge-manual">${s.Type || s.type || ''}</span></td>
                <td style="word-break:break-all">${s.Path || s.path || s.URL || s.url || ''}</td>
                <td class="meta">${(s.Type || s.type) === 'git' ? `branch: ${s.Branch || s.branch || ''}` : `watch: ${s.Watch || s.watch || false}`}</td>
                <td><button class="btn btn-sm" style="background:#c0392b" @click=${() => this._removeSource(i)}>Remove</button></td>
              </tr>`)}
          </tbody>
        </table>
        <details>
          <summary style="cursor:pointer;font-size:0.85rem;color:#7c3aed;user-select:none">+ Add source</summary>
          <div class="cfg-form" style="margin-top:0.75rem">
            <div class="field"><label>Type</label>
              <select id="new-src-type" @change=${e => { this._srcType = e.target.value; }}>
                <option value="local">local</option>
                <option value="git">git</option>
              </select>
            </div>
            ${this._srcType === 'local' ? html`
              <div class="field"><label>Directory path</label>
                <input id="new-src-path" placeholder="/home/you/tasks">
              </div>` : html`
              <div class="field"><label>Repository URL</label>
                <input id="new-src-url" placeholder="https://github.com/you/tasks.git">
              </div>
              <div class="field"><label>Branch</label>
                <input id="new-src-branch" placeholder="main">
              </div>
              <div class="field"><label>Auth token env var (optional)</label>
                <input id="new-src-token-env" placeholder="GITHUB_TOKEN">
              </div>`}
            <button class="btn" @click=${() => this._addSource()}>Add source</button>
          </div>
        </details>
      </div>

      <!-- Raw YAML -->
      <div class="card">
        <div style="display:flex;align-items:center;gap:0.75rem;margin-bottom:0.75rem">
          <h2 style="margin:0">Raw YAML</h2>
          <span style="font-size:0.82rem;color:#888">${this._cfgStatus}</span>
        </div>
        <div id="config-monaco" style="height:400px;border-radius:4px;overflow:hidden;margin-bottom:0.75rem"></div>
        <button class="btn" @click=${() => this._saveRaw()}>&#128190; Save</button>
      </div>`;
  }
}

customElements.define('dc-config', DcConfig);
