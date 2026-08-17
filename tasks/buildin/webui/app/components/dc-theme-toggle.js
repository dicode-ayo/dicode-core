import { LitElement, html } from 'https://esm.sh/lit@3';
import { getCurrentTheme, toggleTheme } from '../lib/theme.js';

class DcThemeToggle extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _theme: { state: true },
  };

  constructor() {
    super();
    this._theme = 'dark';
    this._onThemeChange = (e) => { this._theme = e.detail; };
  }

  connectedCallback() {
    super.connectedCallback();
    this._theme = getCurrentTheme();
    window.addEventListener('dicode-theme-change', this._onThemeChange);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    window.removeEventListener('dicode-theme-change', this._onThemeChange);
  }

  render() {
    const isDark = this._theme === 'dark';
    const label = isDark ? 'Switch to light mode' : 'Switch to dark mode';
    return html`
      <style>
        dc-theme-toggle button {
          background: transparent;
          border: var(--dicode-border-width) solid var(--dicode-border);
          color: var(--dicode-text);
          width: 32px; height: 32px;
          border-radius: var(--dicode-radius-md);
          display: flex; align-items: center; justify-content: center;
          cursor: pointer;
          padding: 0;
          transition: background var(--dicode-duration-fast) var(--dicode-ease), border-color var(--dicode-duration-fast) var(--dicode-ease), color var(--dicode-duration-fast) var(--dicode-ease);
        }
        dc-theme-toggle button:hover {
          background: var(--dicode-card-bg);
          border-color: var(--dicode-sky);
          color: var(--dicode-sky);
        }
        dc-theme-toggle svg { width: 16px; height: 16px; }
      </style>
      <button type="button" aria-label=${label} title=${label} @click=${toggleTheme}>
        ${isDark
          ? html`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>
            </svg>`
          : html`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>
            </svg>`}
      </button>
    `;
  }
}

customElements.define('dc-theme-toggle', DcThemeToggle);
