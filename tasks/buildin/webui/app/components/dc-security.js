import { LitElement, html } from 'https://esm.sh/lit@3';
import { get, post, del } from '../lib/api.js';

class DcSecurity extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _devices:    { state: true },
    _keys:       { state: true },
    _newKeyName: { state: true },
    _newKeyRaw:  { state: true }, // shown once after creation
    _status:     { state: true },
  };

  constructor() {
    super();
    this._devices    = null;
    this._keys       = null;
    this._newKeyName = '';
    this._newKeyRaw  = '';
    this._status     = '';
  }

  connectedCallback() {
    super.connectedCallback();
    this._load();
  }

  async _load() {
    try {
      const [devices, keys] = await Promise.all([
        get('/api/auth/devices'),
        get('/api/auth/keys'),
      ]);
      this._devices = devices;
      this._keys    = keys;
    } catch (e) {
      this._status = 'Failed to load: ' + e.message;
    }
  }

  async _revokeDevice(id) {
    if (!confirm('Remove this trusted device?')) return;
    try {
      await del(`/api/auth/devices/${id}`);
      this._load();
    } catch (e) { alert('Failed: ' + e.message); }
  }

  async _createKey() {
    const name = this._newKeyName.trim();
    if (!name) return;
    try {
      const res = await post('/api/auth/keys', { name });
      this._newKeyRaw  = res.key;
      this._newKeyName = '';
      this._load();
    } catch (e) { alert('Failed: ' + e.message); }
  }

  async _revokeKey(id) {
    if (!confirm('Revoke this API key?')) return;
    try {
      await del(`/api/auth/keys/${id}`);
      if (this._keys) this._keys = this._keys.filter(k => k.id !== id);
    } catch (e) { alert('Failed: ' + e.message); }
  }

  async _logoutAll() {
    if (!confirm('This will revoke all sessions and trusted devices on this server. Continue?')) return;
    try {
      await post('/api/auth/logout-all');
      location.reload();
    } catch (e) { alert('Failed: ' + e.message); }
  }

  _fmtDate(iso) {
    return new Date(iso).toLocaleString(undefined, {
      dateStyle: 'medium', timeStyle: 'short',
    });
  }

  _copyKey() {
    navigator.clipboard.writeText(this._newKeyRaw).catch(() => {});
  }

  // The CLI shape mirrors `dicode mcp install` so what the operator
  // sees here is what the dicode CLI would run on their behalf. URL
  // is rendered against window.location so reverse-proxy + custom-port
  // setups produce a working command without manual editing.
  _claudeMcpAddCmd(rawKey) {
    const url = window.location.origin + '/mcp';
    return `claude mcp add --transport http dicode ${url} --header 'Authorization: Bearer ${rawKey}'`;
  }

  _copyClaudeCmd() {
    navigator.clipboard.writeText(this._claudeMcpAddCmd(this._newKeyRaw)).catch(() => {});
  }

  render() {
    return html`
      <h1>Security</h1>

      <!-- ── Trusted Devices ─────────────────────────────────── -->
      <h2 style="margin-top:1.5rem">Trusted Devices</h2>
      <p class="meta" style="margin-bottom:0.75rem">
        Browsers that were granted long-term access (30 days). Revoke any device
        to require re-authentication on the next visit.
      </p>

      ${!this._devices ? html`<p class="meta">Loading…</p>` : html`
        <table>
          <thead>
            <tr>
              <th>Browser / device</th>
              <th>IP</th>
              <th>Last seen</th>
              <th>Expires</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            ${this._devices.length === 0 ? html`
              <tr><td colspan="5" style="text-align:center;color:var(--muted)">No trusted devices.</td></tr>
            ` : this._devices.map(d => html`
              <tr>
                <td style="font-size:0.8rem;max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title=${d.label}>
                  ${d.label || '—'}
                </td>
                <td><code style="font-size:0.8rem">${d.ip || '—'}</code></td>
                <td style="font-size:0.8rem">${this._fmtDate(d.last_seen)}</td>
                <td style="font-size:0.8rem">${this._fmtDate(d.expires_at)}</td>
                <td style="text-align:right">
                  <button class="btn btn-sm" style="background:var(--red)"
                    @click=${() => this._revokeDevice(d.id)}>Revoke</button>
                </td>
              </tr>
            `)}
          </tbody>
        </table>
      `}

      <!-- ── API Keys ────────────────────────────────────────── -->
      <h2 style="margin-top:2rem">API Keys</h2>
      <p class="meta" style="margin-bottom:0.75rem">
        Bearer tokens for programmatic access (MCP clients, CI integrations).
        The raw key is shown <strong>once</strong> at creation — copy it now.
      </p>

      ${this._newKeyRaw ? html`
        <div class="card" style="background:rgba(166, 227, 161, .15);margin-bottom:var(--space-md)">
          <p style="font-size:0.85rem;font-weight:600;margin-bottom:0.4rem">
            ✓ New API key created — copy it now, it won't be shown again.
          </p>
          <div style="display:flex;gap:var(--space-sm);align-items:center">
            <code style="font-size:0.82rem;word-break:break-all;flex:1">${this._newKeyRaw}</code>
            <button class="btn btn-sm" @click=${() => this._copyKey()}>Copy</button>
            <button class="btn btn-sm secondary" @click=${() => this._newKeyRaw = ''}>Dismiss</button>
          </div>

          <!-- Connect to Claude Code: pre-filled "claude mcp add" command,
               same shape that "dicode mcp install" would run. Operators with
               the "claude" CLI on PATH can paste this straight into a terminal
               and they are connected. NOTE: backticks are forbidden inside
               this comment because it sits inside a lit-html tagged template
               literal that uses backticks as its delimiter. -->
          <details style="margin-top:0.75rem">
            <summary style="cursor:pointer;font-size:0.85rem;font-weight:600">
              ▸ Connect to Claude Code (one-liner)
            </summary>
            <div style="margin-top:0.5rem;display:flex;gap:var(--space-sm);align-items:center">
              <code style="font-size:0.78rem;word-break:break-all;flex:1;background:rgba(0,0,0,.2);padding:0.5rem;border-radius:4px">${this._claudeMcpAddCmd(this._newKeyRaw)}</code>
              <button class="btn btn-sm" @click=${() => this._copyClaudeCmd()}>Copy</button>
            </div>
            <p class="meta" style="font-size:0.75rem;margin-top:0.4rem">
              Requires the official <code>claude</code> CLI on PATH (<code>npm i -g @anthropic-ai/claude-code</code> or
              <code>curl -fsSL https://install.claude.ai | bash</code>). Equivalent to
              <code>dicode mcp install --key ${this._newKeyRaw.slice(0, 8)}…</code>.
            </p>
          </details>
        </div>
      ` : ''}

      <div class="card" style="margin-bottom:var(--space-md)">
        <h2 style="margin-bottom:0.75rem">Create API Key</h2>
        <div style="display:flex;gap:var(--space-sm)">
          <input
            placeholder="Key name (e.g. Claude Desktop)"
            class="input"
            style="flex:1"
            .value=${this._newKeyName}
            @input=${e => this._newKeyName = e.target.value}
            @keydown=${e => { if (e.key === 'Enter') this._createKey(); }}
          >
          <button class="btn" @click=${() => this._createKey()}>Create</button>
        </div>
      </div>

      ${!this._keys ? html`<p class="meta">Loading…</p>` : html`
        <table>
          <thead>
            <tr><th>Name</th><th>Prefix</th><th>Last used</th><th></th></tr>
          </thead>
          <tbody>
            ${this._keys.length === 0 ? html`
              <tr><td colspan="4" style="text-align:center;color:var(--muted)">No API keys.</td></tr>
            ` : this._keys.map(k => html`
              <tr>
                <td>${k.name}</td>
                <td><code style="font-size:0.82rem">${k.prefix}</code></td>
                <td style="font-size:0.82rem">${k.last_used ? this._fmtDate(k.last_used) : '—'}</td>
                <td style="text-align:right">
                  <button class="btn btn-sm" style="background:var(--red)"
                    @click=${() => this._revokeKey(k.id)}>Revoke</button>
                </td>
              </tr>
            `)}
          </tbody>
        </table>
      `}

      <!-- ── Danger Zone ─────────────────────────────────────── -->
      <h2 style="margin-top:2rem;color:var(--red)">Danger Zone</h2>
      <div class="card" style="border:1px solid var(--red)">
        <p style="font-size:0.85rem;margin-bottom:0.75rem">
          Revoke <strong>all</strong> active sessions and trusted devices on this server.
          Every browser will need to re-authenticate.
        </p>
        <button class="btn" style="background:var(--red)" @click=${() => this._logoutAll()}>
          Revoke all sessions &amp; devices
        </button>
      </div>
    `;
  }
}

customElements.define('dc-security', DcSecurity);
