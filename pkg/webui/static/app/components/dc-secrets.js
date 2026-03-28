import { LitElement, html } from 'https://esm.sh/lit@3';
import { get, post, del } from '../lib/api.js';

class DcSecrets extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _secrets: { state: true },
    _locked:  { state: true },
    _status:  { state: true },
  };

  constructor() {
    super();
    this._secrets = null;
    this._locked = false;
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
      this._locked = false;
    } catch(_) {
      this._locked = true;
      this._secrets = null;
    }
  }

  async _unlock() {
    const pw = this.querySelector('#secrets-pw')?.value;
    try {
      await post('/api/secrets/unlock', { password: pw });
      this._load();
    } catch(_) { this._status = 'Incorrect password'; }
  }

  async _lock() {
    try { await post('/api/secrets/lock'); this._load(); } catch(_) {}
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
    if (this._locked) return html`
      <h1>Secrets</h1>
      <div class="card" style="max-width:400px">
        <h2>Unlock Secrets</h2>
        <p class="meta" style="margin-bottom:1rem">Enter your master password to view and edit secrets.</p>
        <input type="password" id="secrets-pw" placeholder="Master password" class="input"
          style="width:100%;margin-bottom:0.5rem"
          @keydown=${e => { if (e.key === 'Enter') this._unlock(); }}>
        <button class="btn" @click=${() => this._unlock()}>Unlock</button>
        <span style="margin-left:0.5rem;font-size:0.85rem;color:red">${this._status}</span>
      </div>`;

    const secrets = this._secrets || [];
    return html`
      <h1>Secrets</h1>
      <div style="display:flex;gap:0.5rem;margin-bottom:1rem">
        <button class="btn btn-sm secondary" @click=${() => this._lock()}>Lock</button>
      </div>
      <div class="card" style="margin-bottom:1rem">
        <h2 style="margin-bottom:0.75rem">Add Secret</h2>
        <div style="display:flex;gap:0.5rem;flex-wrap:wrap">
          <input id="secret-key" placeholder="KEY_NAME" class="input" style="font-family:monospace">
          <input id="secret-value" type="password" placeholder="value" class="input" style="flex:1;min-width:200px">
          <button class="btn" @click=${() => this._add()}>Add</button>
        </div>
      </div>
      <table>
        <thead><tr><th>Key</th><th></th></tr></thead>
        <tbody>
          ${secrets.length === 0 ? html`
            <tr><td colspan="2" style="text-align:center;color:#888">No secrets stored.</td></tr>
          ` : secrets.map(k => html`
            <tr>
              <td><code>${k}</code></td>
              <td style="text-align:right">
                <button class="btn btn-sm" style="background:#dc3545" @click=${() => this._delete(k)}>Delete</button>
              </td>
            </tr>`)}
        </tbody>
      </table>`;
  }
}

customElements.define('dc-secrets', DcSecrets);
