import { LitElement, html } from "https://esm.sh/lit@3";

// dc-provider-card — one row in the providers list. Three columns:
//   icon (brand-colored monochrome SVG via mask-image)
//   main (label + scope or error)
//   actions (status pill + Connect/Reconnect button)
// On Connect click, dispatches a "connect" CustomEvent with detail.provider.
//
// Lit's html`` template literal auto-escapes interpolated values, so
// arbitrary strings inside ${row.scope}, ${meta.label}, etc. cannot
// inject HTML or script content. The two `style="..."` interpolations
// (icon's --brand and --icon-url) go through safeColor() and a fixed
// per-row asset path respectively.
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
    // Icon path is per-provider; safeProviderKey ensures only the lowercase
    // alnum/underscore characters from the daemon-sanitized provider name
    // can appear in the URL — no traversal, no protocol-injection.
    //
    // Use an absolute path rooted at the daemon-injected hook base (read from
    // <meta name="dicode-hook">). Relative URLs ("app/icons/...") set as
    // CSS custom properties in inline style are resolved against the
    // CONSUMING stylesheet's location, not the document base — that produced
    // a doubled "app/app/" prefix when used inside theme.css.
    const iconUrl = `url("${HOOK_BASE}/app/icons/${safeProviderKey(row.provider)}.svg")`;
    const brand = safeColor(meta.color);
    return html`
      <span class="provider-icon"
            aria-hidden="true"
            style="--brand:${brand};--icon-url:${iconUrl}"></span>
      <div class="provider-main">
        <span class="provider-name">${meta.label}</span>
        ${row.scope ? html`
          <span class="provider-scope">scope: <code>${row.scope}</code></span>
        ` : ""}
        ${this.error ? html`
          <span class="provider-error">${this.error}</span>
        ` : ""}
      </div>
      <div class="provider-actions">
        ${this._pill(row)}
        <button class="btn"
                aria-label="${buttonLabel} ${meta.label}"
                @click=${() => this._onConnect()}>${buttonLabel}</button>
      </div>
    `;
  }
}

// HOOK_BASE is the URL prefix of this task's webhook (e.g. /hooks/auth-providers,
// or /u/<relay-uuid>/hooks/auth-providers when served through the relay).
// Read from the <meta name="dicode-hook"> tag the trigger engine injects into
// every webhook UI; falls back to the canonical literal if the tag is missing.
const HOOK_BASE = (() => {
  const m = document.querySelector('meta[name="dicode-hook"]');
  const v = m?.getAttribute("content");
  return v && /^[/A-Za-z0-9_\-]+$/.test(v) ? v.replace(/\/$/, "") : "/hooks/auth-providers";
})();

// safeColor returns the input only if it's a hex color literal (#rgb,
// #rrggbb, or #rrggbbaa). Anything else falls back to a neutral grey.
// Defends against future contributors loading meta.color from
// untrusted config — Lit's html`` does NOT escape CSS inside style="".
function safeColor(c) {
  return /^#[0-9a-f]{3,8}$/i.test(c ?? "") ? c : "#888";
}

// safeProviderKey constrains the value to lowercase a-z, 0-9, underscore —
// the daemon's sanitizeProviderPrefix already uppercases similar input,
// but we re-validate client-side before interpolating into a URL because
// Lit does not auto-escape inside CSS string values like url("...").
function safeProviderKey(k) {
  return /^[a-z0-9_]{2,}$/.test(k ?? "") ? k : "unknown";
}

function humanDelta(ms) {
  const sec = Math.floor(ms / 1000);
  if (sec < 60)    return `in ${sec}s`;
  if (sec < 3600)  return `in ${Math.floor(sec / 60)}m`;
  if (sec < 86400) return `in ${Math.floor(sec / 3600)}h`;
  return `in ${Math.floor(sec / 86400)}d`;
}

customElements.define("dc-provider-card", DcProviderCard);
