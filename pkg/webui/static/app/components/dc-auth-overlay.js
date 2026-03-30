import { LitElement, html } from 'https://esm.sh/lit@3';

// dc-auth-overlay is injected once into the DOM by app.js.
// When the API receives a 401 it calls overlay.show(resolveFn).
// After successful login the resolveFn is called so the original
// API request can be retried transparently.
class DcAuthOverlay extends LitElement {
  createRenderRoot() { return this; }

  static properties = {
    _visible: { state: true },
    _error:   { state: true },
    _loading: { state: true },
  };

  constructor() {
    super();
    this._visible = false;
    this._error   = '';
    this._loading = false;
    this._resolve = null; // set by show()
  }

  // Called by api.js when a 401 is received.
  show(resolve) {
    this._resolve = resolve;
    this._error   = '';
    this._loading = false;
    this._visible = true;
    // Focus the password field after render.
    this.updateComplete.then(() => {
      this.querySelector('#auth-pw')?.focus();
    });
  }

  async _submit() {
    const pw    = this.querySelector('#auth-pw')?.value ?? '';
    const trust = this.querySelector('#auth-trust')?.checked ?? false;
    if (!pw) return;
    this._loading = true;
    this._error   = '';
    try {
      const res = await fetch('/api/secrets/unlock', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: pw, trust }),
      });
      if (!res.ok) {
        const msg = await res.text();
        this._error = msg.includes('incorrect') ? 'Incorrect password.' : msg;
        return;
      }
      this._visible = false;
      this._resolve?.();
      this._resolve = null;
      // Reset the field so it doesn't auto-fill next time.
      const field = this.querySelector('#auth-pw');
      if (field) field.value = '';
    } catch (e) {
      this._error = 'Network error — ' + e.message;
    } finally {
      this._loading = false;
    }
  }

  _onKeydown(e) {
    if (e.key === 'Enter') this._submit();
    if (e.key === 'Escape') { /* keep overlay open — don't dismiss on Escape */ }
  }

  render() {
    if (!this._visible) return html``;
    return html`
      <div style="
        position:fixed;inset:0;z-index:9999;
        background:rgba(0,0,0,0.55);backdrop-filter:blur(2px);
        display:flex;align-items:center;justify-content:center;
      ">
        <div style="
          background:#fff;border-radius:10px;padding:2rem 2.25rem;
          width:100%;max-width:380px;box-shadow:0 8px 32px rgba(0,0,0,.25);
        ">
          <div style="font-size:1.5rem;margin-bottom:0.25rem">&#9889; dicode</div>
          <p style="color:#555;font-size:0.9rem;margin-bottom:1.25rem">
            Enter your passphrase to continue.
          </p>

          <input
            id="auth-pw"
            type="password"
            placeholder="Passphrase"
            class="input"
            style="width:100%;margin-bottom:0.75rem;font-size:1rem;padding:0.5rem 0.6rem"
            @keydown=${this._onKeydown}
            ?disabled=${this._loading}
          >

          <label style="display:flex;align-items:center;gap:0.5rem;font-size:0.85rem;color:#555;margin-bottom:1rem;cursor:pointer">
            <input id="auth-trust" type="checkbox" style="width:auto">
            Trust this browser for 30 days
          </label>

          ${this._error ? html`
            <p style="color:#dc3545;font-size:0.85rem;margin-bottom:0.75rem">${this._error}</p>
          ` : ''}

          <button
            class="btn"
            style="width:100%;padding:0.55rem;font-size:0.95rem"
            @click=${this._submit}
            ?disabled=${this._loading}
          >
            ${this._loading ? 'Signing in…' : 'Sign in'}
          </button>
        </div>
      </div>
    `;
  }
}

customElements.define('dc-auth-overlay', DcAuthOverlay);
