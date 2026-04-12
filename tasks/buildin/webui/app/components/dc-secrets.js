import { LitElement, html } from 'https://esm.sh/lit@3';
import { get, post, del } from '../lib/api.js';

class DcSecrets extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _secrets: { state: true },
    _status:  { state: true },
  };

  constructor() {
    super();
    this._secrets = null;
    this._status = '';
  }

  connectedCallback() {
    super.connectedCallback();
    this._load();
  }

  async _load() {
    this._status = '';
    try {
      this._secrets = await get('/api/secrets');
    } catch(e) {
      this._status = 'Failed to load secrets: ' + e.message;
      this._secrets = [];
    }
  }

  async _add() {
    const key   = this.querySelector('#secret-key')?.value;
    const value = this.querySelector('#secret-value')?.value;
    if (!key) return;
    try { await post('/api/secrets', { key, value }); this._load(); }
    catch(e) { alert('Failed: ' + e.message); }
  }

  async _delete(key) {
    if (!confirm(`Delete secret "${key}"?`)) return;
    try { await del(`/api/secrets/${key}`); this._load(); }
    catch(e) { alert('Failed: ' + e.message); }
  }

  render() {
    const secrets = this._secrets || [];
    return html`
      <h1>Secrets</h1>
      <div class="card" style="margin-bottom:var(--space-md)">
        <h2 style="margin-bottom:0.75rem">Add Secret</h2>
        <div style="display:flex;gap:var(--space-sm);flex-wrap:wrap">
          <input id="secret-key" placeholder="KEY_NAME" class="input" style="font-family:monospace">
          <input id="secret-value" type="password" placeholder="value" class="input" style="flex:1;min-width:200px">
          <button class="btn" @click=${() => this._add()}>Add</button>
        </div>
      </div>
      ${this._status ? html`<p style="color:red;margin-bottom:var(--space-md)">${this._status}</p>` : ''}
      <table>
        <thead><tr><th>Key</th><th></th></tr></thead>
        <tbody>
          ${secrets.length === 0 ? html`
            <tr><td colspan="2" style="text-align:center;color:var(--muted)">No secrets stored.</td></tr>
          ` : secrets.map(k => html`
            <tr>
              <td><code>${k}</code></td>
              <td style="text-align:right">
                <button class="btn btn-sm" style="background:var(--red)" @click=${() => this._delete(k)}>Delete</button>
              </td>
            </tr>`)}
        </tbody>
      </table>`;
  }
}

customElements.define('dc-secrets', DcSecrets);
