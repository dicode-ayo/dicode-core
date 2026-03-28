import { LitElement, html } from 'https://esm.sh/lit@3';
import { wsOn } from '../lib/ws.js';

const LEVEL_COLOR = { DEBUG: '#89b4fa', INFO: '#a6e3a1', WARN: '#f9e2af', ERROR: '#f38ba8' };

class DcLogBar extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _open:      { state: true },
    _connected: { state: true },
    _count:     { state: true },
  };

  constructor() {
    super();
    this._open = false;
    this._connected = false;
    this._count = 0;
    this._lines = []; // raw DOM spans — appended directly for perf
  }

  connectedCallback() {
    super.connectedCallback();
    wsOn('log:line', d => this._append(d.line));
    wsOn('ws:status', d => { this._connected = d.connected; });
  }

  _append(line) {
    this._count++;
    let text = line, color = '';
    try {
      const j = JSON.parse(line);
      const lvl = (j.level || 'info').toUpperCase();
      const ts = j.ts ? new Date(j.ts * 1000).toLocaleTimeString() : '';
      const extra = Object.entries(j)
        .filter(([k]) => !['level','ts','msg','caller','stacktrace'].includes(k))
        .map(([k, v]) => k + '=' + JSON.stringify(v)).join(' ');
      text = ts + ' ' + lvl.padEnd(5) + ' ' + (j.msg || '') + (extra ? ' ' + extra : '');
      color = LEVEL_COLOR[lvl] || '';
    } catch(_) {}

    const console = this.querySelector('#logconsole');
    if (console) {
      const span = document.createElement('span');
      if (color) span.style.color = color;
      span.textContent = text + '\n';
      console.appendChild(span);
      if (this._open) console.scrollTop = console.scrollHeight;
    }
    // Update count display without full re-render
    const cnt = this.querySelector('#logcount');
    if (cnt) cnt.textContent = this._count + ' lines';
  }

  _toggle() {
    this._open = !this._open;
    if (this._open) {
      this.updateComplete.then(() => {
        const el = this.querySelector('#logconsole');
        if (el) el.scrollTop = el.scrollHeight;
      });
    }
  }

  render() {
    const statusText  = this._connected ? '● connected'    : '● disconnected';
    const statusColor = this._connected ? '#a6e3a1'        : '#f38ba8';

    return html`
      <div style="position:fixed;bottom:0;left:0;right:0;background:#1a1a2e;color:#cdd6f4;font-family:monospace;font-size:0.78rem;z-index:100;border-top:1px solid #333;">
        <div @click=${() => this._toggle()}
          style="padding:0.3rem 1rem;cursor:pointer;background:#16213e;display:flex;align-items:center;gap:0.5rem;user-select:none;">
          <span>${this._open ? '▼' : '▶'}</span> App Logs
          <span style="margin-left:0.5rem;font-size:0.7rem;color:${statusColor}">${statusText}</span>
          <span id="logcount" style="margin-left:auto;font-size:0.7rem;color:#666">${this._count ? this._count + ' lines' : ''}</span>
        </div>
        <pre id="logconsole"
          style="display:${this._open ? 'block' : 'none'};height:200px;overflow-y:auto;padding:0.5rem 1rem;margin:0;white-space:pre-wrap;word-break:break-all"></pre>
      </div>
      <div style="height:2rem"></div>`;
  }
}

customElements.define('dc-log-bar', DcLogBar);
