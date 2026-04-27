import { LitElement, html } from "https://esm.sh/lit@3";
import { api } from "../lib/api.js";
import "./dc-provider-card.js";

const POLL_INTERVAL_MS = 5_000;

class DcProvidersPage extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _rows:   { state: true },
    _status: { state: true },
  };

  constructor() {
    super();
    this._rows = null;
    this._status = "loading";
    this._timer = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._refresh();
    this._timer = setInterval(() => this._refresh(), POLL_INTERVAL_MS);
    this.addEventListener("connect", (e) => this._onConnect(e));
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    if (this._timer) clearInterval(this._timer);
  }

  async _refresh() {
    try {
      const rows = await api.list();
      this._rows = Array.isArray(rows) ? rows : [];
      this._status = "ready";
    } catch (err) {
      this._status = `error: ${err.message || err}`;
    }
  }

  async _onConnect(e) {
    const provider = e.detail?.provider;
    if (!provider) return;
    const card = e.target;
    try {
      const out = await api.connect(provider);
      if (!out?.url) throw new Error("provider task did not return a url");
      window.open(out.url, "_blank", "noopener");
    } catch (err) {
      if (typeof card.setError === "function") {
        card.setError(err.message || String(err));
      }
    }
  }

  _renderBody() {
    if (this._status === "loading") {
      return html`<p style="color:var(--muted)">Loading…</p>`;
    }
    if (this._status.startsWith("error:")) {
      return html`<p style="color:var(--pill-err)">${this._status}</p>`;
    }
    const rows = this._rows ?? [];
    if (rows.length === 0) {
      return html`<p style="color:var(--muted)">No providers configured. Set the <code>providers</code> param on this task to enable some.</p>`;
    }
    return html`
      <ul class="providers-grid">
        ${rows.map(row => html`
          <li><dc-provider-card .row=${row}></dc-provider-card></li>
        `)}
      </ul>
    `;
  }

  render() {
    return html`
      <header>
        <h1>OAuth providers</h1>
        <p style="color:var(--muted);margin:0;max-width:640px">
          Click <strong>Connect</strong> on a provider to start an authorisation flow.
          The card flips to <strong>Connected</strong> automatically once the token lands in the secrets store.
        </p>
      </header>
      <main style="margin-top:1.25rem">
        ${this._renderBody()}
      </main>
    `;
  }
}

customElements.define("dc-providers-page", DcProvidersPage);
