import { LitElement, html } from 'https://esm.sh/lit@3';
import { wsOn } from '../lib/ws.js';
import { navigate } from '../lib/router.js';

const STORAGE_KEY = 'dicode_notifs';
const SW_KEY      = 'dicode_notif_banner_dismissed';

class DcNotifPanel extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _open:   { state: true },
    _unread: { state: true },
    _notifs: { state: true },
    _banner: { state: true },
  };

  constructor() {
    super();
    this._open = false;
    this._unread = 0;
    this._notifs = this._load();
    this._banner = false;
    this._swReg = null;
  }

  _load() {
    try { return JSON.parse(localStorage.getItem(STORAGE_KEY) || '[]'); } catch(_) { return []; }
  }

  _save(list) {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(list.slice(-50)));
  }

  connectedCallback() {
    super.connectedCallback();
    wsOn('run:finished', d => this._addNotif(d));
    this._maybeShowBanner();
    this._registerSW();
  }

  _addNotif(evt) {
    const list = this._load();
    list.push({ ts: Date.now(), runID: evt.runID, taskName: evt.taskName, taskID: evt.taskID, status: evt.status, durationMs: evt.durationMs });
    this._save(list);
    this._notifs = [...list];
    if (!this._open) this._unread++;
    if (this._swReg?.active && Notification.permission === 'granted') {
      this._swReg.active.postMessage({ type: 'run:complete', ...evt });
    }
  }

  _toggle() {
    this._open = !this._open;
    if (this._open) { this._unread = 0; }
  }

  _clear() {
    this._save([]);
    this._notifs = [];
  }

  _maybeShowBanner() {
    if (typeof Notification === 'undefined') return;
    if (Notification.permission === 'granted') return;
    if (localStorage.getItem(SW_KEY)) return;
    this._banner = true;
  }

  async _requestPermission() {
    await Notification.requestPermission();
    this._banner = false;
    localStorage.setItem(SW_KEY, '1');
    this._registerSW();
  }

  _dismissBanner() {
    localStorage.setItem(SW_KEY, '1');
    this._banner = false;
  }

  _registerSW() {
    if (!('serviceWorker' in navigator)) return;
    navigator.serviceWorker.register('/sw.js', { scope: '/' })
      .then(reg => { this._swReg = reg; })
      .catch(() => {});
  }

  render() {
    const notifs = [...(this._notifs || [])].reverse();

    return html`
      <!-- Bell icon -->
      <div @click=${() => this._toggle()}
        style="margin-left:auto;cursor:pointer;position:relative;padding:0.25rem 0.5rem;color:#ccc;font-size:1.1rem"
        title="Notifications">
        &#128276;
        ${this._unread > 0 ? html`
          <span style="position:absolute;top:0;right:0;background:#f38ba8;color:#fff;border-radius:9px;font-size:0.65rem;padding:1px 5px;font-weight:700">
            ${this._unread > 99 ? '99+' : this._unread}
          </span>` : ''}
      </div>

      <!-- Inbox panel -->
      ${this._open ? html`
        <div style="display:block;position:fixed;top:48px;right:0;width:340px;max-height:480px;overflow-y:auto;background:#fff;border:1px solid #dee2e6;border-radius:6px 0 0 6px;box-shadow:-2px 2px 8px rgba(0,0,0,.15);z-index:200">
          <div style="display:flex;align-items:center;padding:0.6rem 1rem;border-bottom:1px solid #eee;background:#f8f9fa">
            <strong style="font-size:0.9rem">Notifications</strong>
            <button @click=${() => this._clear()} style="margin-left:auto;background:none;border:none;cursor:pointer;color:#6c757d;font-size:0.8rem">Clear</button>
          </div>
          <div style="font-size:0.85rem">
            ${notifs.length === 0 ? html`
              <div style="padding:1rem;color:#888;text-align:center">No notifications yet.</div>
            ` : notifs.map(n => {
              const ago = Math.round((Date.now() - n.ts) / 60000);
              const agoStr = ago < 1 ? 'just now' : ago < 60 ? ago + 'm ago' : Math.round(ago / 60) + 'h ago';
              const ok = n.status === 'success';
              const bg = ok ? '#d1e7dd' : '#f8d7da';
              const color = ok ? '#0f5132' : '#842029';
              return html`
                <div style="display:flex;align-items:center;gap:0.5rem;padding:0.6rem 1rem;border-bottom:1px solid #f0f0f0">
                  <span style="background:${bg};color:${color};border-radius:4px;padding:0.1em 0.5em;font-weight:600;font-size:0.78rem">
                    ${ok ? '✓' : '✗'} ${n.status}
                  </span>
                  <div style="flex:1;min-width:0">
                    <div style="font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${n.taskName}</div>
                    <div style="color:#888;font-size:0.75rem">${agoStr}</div>
                  </div>
                  <a href="/runs/${n.runID}" style="font-size:0.75rem;white-space:nowrap"
                    @click=${e => { e.preventDefault(); this._open = false; navigate('/runs/' + n.runID); }}>View →</a>
                </div>`;
            })}
          </div>
        </div>` : ''}

      <!-- Permission banner -->
      ${this._banner ? html`
        <div style="display:flex;position:fixed;bottom:2.5rem;left:0;right:0;background:#1a1a2e;color:#cdd6f4;padding:0.6rem 1.5rem;align-items:center;gap:1rem;z-index:150;border-top:1px solid #333">
          <span style="flex:1">&#9889; dicode can send browser notifications when tasks complete.</span>
          <button class="btn btn-sm" @click=${() => this._requestPermission()}>Allow</button>
          <button @click=${() => this._dismissBanner()} style="background:none;border:none;cursor:pointer;color:#888;font-size:1.1rem">✕</button>
        </div>` : ''}`;
  }
}

customElements.define('dc-notif-panel', DcNotifPanel);
