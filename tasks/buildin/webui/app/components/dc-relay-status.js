import { LitElement, html, css } from 'https://esm.sh/lit@3';
import { get } from '../lib/api.js';

// How long ago `ts` was, in relaxed English ("just now", "3m ago"…).
function relTime(ts) {
  if (!ts) return '';
  const t = new Date(ts).getTime();
  if (!t) return '';
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (s < 30) return 'just now';
  if (s < 90) return '1m ago';
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 5400) return '1h ago';
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

class DcRelayStatus extends LitElement {
  static styles = css`
    :host { display: inline-flex; }
    .pill {
      display: inline-flex; align-items: center; gap: 0.35rem;
      padding: 0.2rem 0.55rem;
      border-radius: 999px;
      font-size: 0.78rem;
      font-weight: 500;
      border: 1px solid transparent;
      cursor: help;
      user-select: none;
    }
    .pill.ok  { background: rgba(46, 160, 67, 0.14); color: #3fb950; border-color: rgba(46, 160, 67, 0.35); }
    .pill.err { background: rgba(248, 81, 73, 0.14); color: #f85149; border-color: rgba(248, 81, 73, 0.35); }
    .pill.off { background: rgba(140, 150, 163, 0.10); color: #8c96a3; border-color: rgba(140, 150, 163, 0.28); }
    .dot { width: 0.55rem; height: 0.55rem; border-radius: 50%; background: currentColor; }
  `;

  static properties = { _status: { state: true } };

  constructor() {
    super();
    this._status = null;
    this._timer = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._poll();
    this._timer = setInterval(() => this._poll(), 5000);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    if (this._timer) clearInterval(this._timer);
    this._timer = null;
  }

  async _poll() {
    try {
      this._status = await get('/api/relay/status');
    } catch (_) {
      // Stay quiet on transient fetch errors — the poll retries in 5s.
    }
  }

  _tooltip() {
    const s = this._status;
    if (!s) return '';
    const rel = relTime(s.since);
    if (s.connected) {
      return `${s.remote_url || 'relay'} · connected ${rel}`.trim();
    }
    const parts = [s.remote_url || 'relay', `disconnected ${rel}`.trim()];
    if (s.last_error) parts.push(s.last_error);
    if (s.reconnect_attempts) parts.push(`${s.reconnect_attempts} retries`);
    return parts.filter(Boolean).join(' · ');
  }

  render() {
    const s = this._status;
    // Always render the pill — a grey "off" state for disabled relays
    // gives operators a stable spot to click through and surfaces
    // misconfiguration ("I thought I enabled it?") instead of hiding it.
    if (!s) {
      return html`<span class="pill off" title="loading relay status…"><span class="dot"></span>Relay: …</span>`;
    }
    if (!s.enabled) {
      return html`<span class="pill off" title="relay disabled in config; set relay.enabled: true in dicode.yaml"><span class="dot"></span>Relay: off</span>`;
    }
    const cls = s.connected ? 'ok' : 'err';
    const label = s.connected ? 'Relay' : 'Relay offline';
    return html`<span class="pill ${cls}" title=${this._tooltip()}>
      <span class="dot"></span>${label}
    </span>`;
  }
}

customElements.define('dc-relay-status', DcRelayStatus);
