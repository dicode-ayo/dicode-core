import { LitElement, html } from 'https://esm.sh/lit@3';

// dc-toast listens for `dc-toast` window CustomEvents and renders transient
// notifications stacked at the bottom-right of the viewport. Other components
// fire toasts by dispatching:
//
//   window.dispatchEvent(new CustomEvent('dc-toast', { detail: { message: '...' } }));
//
// Optional detail.kind ('error' | 'info', default 'info') tints the bar.
// Each toast auto-dismisses after 5 seconds and can be dismissed manually.

const TOAST_TTL_MS = 5000;
const VALID_KINDS = new Set(['info', 'error']);

class DcToast extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _toasts: { state: true },
  };

  constructor() {
    super();
    this._toasts = [];
    this._nextID = 0;
    this._onToast = (e) => this._add(e.detail || {});
  }

  connectedCallback() {
    super.connectedCallback();
    window.addEventListener('dc-toast', this._onToast);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    window.removeEventListener('dc-toast', this._onToast);
  }

  _add({ message, kind }) {
    if (!message) return;
    const id = this._nextID++;
    const safeKind = VALID_KINDS.has(kind) ? kind : 'info';
    this._toasts = [...this._toasts, { id, message: String(message), kind: safeKind }];
    setTimeout(() => this._dismiss(id), TOAST_TTL_MS);
  }

  _dismiss(id) {
    this._toasts = this._toasts.filter((t) => t.id !== id);
  }

  render() {
    return html`
      <style>
        dc-toast {
          position: fixed;
          right: 1rem;
          bottom: 1rem;
          display: flex;
          flex-direction: column;
          gap: 0.5rem;
          z-index: 9999;
          pointer-events: none;
        }
        dc-toast .toast {
          pointer-events: auto;
          background: var(--card-bg, #1e1e1e);
          color: var(--text, #ddd);
          border: 1px solid var(--border, #333);
          border-left: 3px solid var(--sky, #4caf50);
          border-radius: var(--radius-md, 4px);
          padding: 0.6rem 0.9rem;
          max-width: 360px;
          box-shadow: 0 4px 12px rgba(0,0,0,.35);
          font-size: 0.9rem;
          display: flex;
          align-items: flex-start;
          gap: 0.5rem;
          animation: dc-toast-in .15s ease-out;
        }
        dc-toast .toast.error { border-left-color: #e57373; }
        dc-toast .toast .msg { flex: 1; word-break: break-word; }
        dc-toast .toast button {
          background: none;
          border: none;
          color: inherit;
          cursor: pointer;
          font-size: 1.1rem;
          line-height: 1;
          padding: 0;
          opacity: 0.6;
        }
        dc-toast .toast button:hover { opacity: 1; }
        @keyframes dc-toast-in {
          from { transform: translateY(8px); opacity: 0; }
          to   { transform: translateY(0);   opacity: 1; }
        }
      </style>
      ${this._toasts.map((t) => html`
        <div class=${`toast ${t.kind}`} role="status">
          <span class="msg">${t.message}</span>
          <button type="button" aria-label="Dismiss" @click=${() => this._dismiss(t.id)}>×</button>
        </div>
      `)}
    `;
  }
}

customElements.define('dc-toast', DcToast);
