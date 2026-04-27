import { LitElement, html } from "https://esm.sh/lit@3";

// dc-provider-card — one row in the providers list. Renders label,
// color dot, status pill, expiry, scope, and a Connect/Reconnect button.
// On Connect click, dispatches a "connect" CustomEvent with detail.provider.
//
// Lit's html`` template literal auto-escapes interpolated values, so
// arbitrary strings inside ${row.scope}, ${meta.label}, etc. cannot
// inject HTML or script content.
class DcProviderCard extends LitElement {
  // Render into the light DOM so theme.css variables apply directly.
  createRenderRoot() { return this; }

  static properties = {
    row:    { attribute: false },
    error:  { state: true },
  };

  constructor() {
    super();
    this.row = null;
    this.error = "";
  }

  setError(msg) { this.error = String(msg || ""); }

  _onConnect() {
    this.error = "";
    this.dispatchEvent(new CustomEvent("connect", {
      bubbles: true,
      detail: { provider: this.row?.provider },
    }));
  }

  _pill(row) {
    if (!row.has_token) {
      return html`<span class="pill" style="background:var(--pill-err)">Not connected</span>`;
    }
    if (!row.expires_at) {
      return html`<span class="pill" style="background:var(--pill-ok)">Connected</span>`;
    }
    const ms = Date.parse(row.expires_at) - Date.now();
    if (Number.isNaN(ms)) {
      return html`<span class="pill" style="background:var(--pill-ok)">Connected</span>`;
    }
    if (ms <= 0) {
      return html`<span class="pill" style="background:var(--pill-err)">Expired</span>`;
    }
    const color = ms < 24 * 3600_000 ? "var(--pill-warn)" : "var(--pill-ok)";
    return html`<span class="pill" style="background:${color}">Expires ${humanDelta(ms)}</span>`;
  }

  render() {
    const row = this.row;
    if (!row) return html``;
    const meta = row.meta || { label: row.provider, color: "#888" };
    const buttonLabel = row.has_token ? "Reconnect" : "Connect";
    return html`
      <div style="display:flex;align-items:center;gap:.5rem;margin-bottom:.5rem">
        <span class="color-dot" style="background:${meta.color}"></span>
        <strong>${meta.label}</strong>
        ${this._pill(row)}
      </div>
      ${row.scope ? html`
        <p style="color:var(--muted);margin:.25rem 0;font-size:.85em">scope: <code>${row.scope}</code></p>
      ` : ""}
      <button class="btn" @click=${() => this._onConnect()}>${buttonLabel}</button>
      ${this.error ? html`
        <p style="color:var(--pill-err);font-size:.85em;margin:.5rem 0 0">${this.error}</p>
      ` : ""}
    `;
  }
}

function humanDelta(ms) {
  const sec = Math.floor(ms / 1000);
  if (sec < 60)    return `in ${sec}s`;
  if (sec < 3600)  return `in ${Math.floor(sec / 60)}m`;
  if (sec < 86400) return `in ${Math.floor(sec / 3600)}h`;
  return `in ${Math.floor(sec / 86400)}d`;
}

customElements.define("dc-provider-card", DcProviderCard);
