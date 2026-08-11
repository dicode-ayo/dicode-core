import { LitElement, html, css } from 'https://esm.sh/lit@3';

// <dc-card> — Stage 1 primitive (#93): a bordered surface with an optional
// title/actions header, replacing the ad hoc `<div class="card" style="...">`
// blocks scattered across dc-task-detail.js/dc-run-detail.js/etc.
//
// Unlike the app's existing light-DOM `dc-*` components, primitives use
// Shadow DOM for real style encapsulation. Theming still flows through —
// custom-property values (var(--card-bg), var(--border), ...) cross the
// shadow boundary even though rules don't, so this stays in sync with
// global.css's `.card` look without importing global.css itself. See
// docs/concepts/webui-components.md.
//
// Slots:
//   title    — header content; falls back to a plain <h2> from `heading`
//   actions  — right-aligned header controls (buttons, icon-buttons, ...)
//   (default) — body content
//
// Props:
//   heading — plain-text heading used when no `title` slot content is given
//   pad     — 'md' (default) | 'none' — body padding
class DcCard extends LitElement {
  static properties = {
    heading: { type: String },
    pad: { type: String },
    _hasTitle: { state: true },
    _hasActions: { state: true },
  };

  static styles = css`
    :host {
      display: block;
      background: var(--card-bg, rgba(255, 255, 255, .04));
      border: 1px solid var(--border, rgba(160, 196, 255, .15));
      border-radius: var(--radius, 14px);
      transition: border-color .2s var(--ease, ease);
    }
    :host(:hover) { border-color: var(--border-strong, rgba(160, 196, 255, .35)); }
    .header {
      display: flex;
      align-items: center;
      gap: var(--space-md, 1rem);
      padding: var(--space-md, 1rem) var(--space-lg, 1.5rem);
      border-bottom: 1px solid var(--border, rgba(160, 196, 255, .15));
    }
    .header[hidden] { display: none; }
    /* Scoped to slot[name="title"] specifically, not ".header
       ::slotted(h2)" generically — .header also contains the "actions"
       slot, and a bare ".header ::slotted(h2)" would match an <h2>
       slotted into *either* one, leaking the title's heading styling
       onto whatever an "actions" consumer put there. */
    slot[name="title"]::slotted(h2), h2 {
      margin: 0;
      font-size: var(--text-lg, 1.15rem);
      font-weight: var(--font-bold, 700);
      color: var(--heading, #fff);
      line-height: var(--leading-snug, 1.3);
    }
    .actions {
      margin-left: auto;
      display: flex;
      align-items: center;
      gap: var(--space-sm, .5rem);
    }
    .body { padding: var(--space-md, 1rem) var(--space-lg, 1.5rem); }
    .body.pad-none { padding: 0; }
  `;

  constructor() {
    super();
    this.heading = '';
    this.pad = 'md';
    this._hasTitle = false;
    this._hasActions = false;
  }

  _onTitleSlotChange(e) {
    this._hasTitle = e.target.assignedNodes({ flatten: true }).length > 0;
  }

  _onActionsSlotChange(e) {
    this._hasActions = e.target.assignedNodes({ flatten: true }).length > 0;
  }

  render() {
    const showHeader = !!this.heading || this._hasTitle || this._hasActions;
    return html`
      <div class="header" ?hidden=${!showHeader}>
        <slot name="title" @slotchange=${this._onTitleSlotChange}>
          ${this.heading ? html`<h2>${this.heading}</h2>` : ''}
        </slot>
        <div class="actions"><slot name="actions" @slotchange=${this._onActionsSlotChange}></slot></div>
      </div>
      <div class="body ${this.pad === 'none' ? 'pad-none' : ''}">
        <slot></slot>
      </div>
    `;
  }
}

customElements.define('dc-card', DcCard);
