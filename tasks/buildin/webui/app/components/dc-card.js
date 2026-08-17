import { LitElement, html, css } from 'https://esm.sh/lit@3';
import { hasSlottedElement } from '../lib/slot-utils.js';

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
    pad: { type: String, reflect: true },
    _hasTitle: { state: true },
    _hasActions: { state: true },
  };

  static styles = css`
    :host {
      display: block;
      background: var(--card-bg);
      border: 1px solid var(--border);
      border-radius: var(--radius);
      /* .2s bypasses theme.css's own --duration-fast/--duration tokens —
         faithfully matching global.css's prevailing (also-untokenized)
         pattern rather than inventing a new one here. Tracked in #710
         alongside global.css's own instances, to fix once and consistently
         rather than renegotiate per component. */
      transition: border-color .2s var(--ease);
    }
    :host(:hover) { border-color: var(--border-strong); }
    .header {
      display: flex;
      align-items: center;
      gap: var(--space-md);
      padding: var(--space-md) var(--space-lg);
      border-bottom: 1px solid var(--border);
    }
    .header[hidden] { display: none; }
    /* Scoped to slot[name="title"] specifically, not ".header
       ::slotted(h2)" generically — .header also contains the "actions"
       slot, and a bare ".header ::slotted(h2)" would match an <h2>
       slotted into *either* one, leaking the title's heading styling
       onto whatever an "actions" consumer put there. */
    slot[name="title"]::slotted(h2), h2 {
      margin: 0;
      font-size: var(--text-lg);
      font-weight: var(--font-bold);
      color: var(--heading);
      line-height: var(--leading-snug);
    }
    .actions {
      margin-inline-start: auto;
      display: flex;
      align-items: center;
      gap: var(--space-sm);
    }
    .body { padding: var(--space-md) var(--space-lg); }
    :host([pad='none']) .body { padding: 0; }
  `;

  constructor() {
    super();
    this.heading = '';
    this.pad = 'md';
    this._hasTitle = false;
    this._hasActions = false;
  }

  // Without this, `_hasTitle`/`_hasActions` stay at their constructor
  // defaults (false) until the title/actions `slotchange` listeners fire
  // — which happens one render *after* the first, since a `<slot>` has
  // to exist in the rendered shadow tree before it can report an
  // assignment. A card with slotted title/actions content but no
  // `heading` attribute (e.g. dc-ui-kit-demo.js's #card-slots-demo) would
  // render its header hidden on the very first paint, then flash it in
  // once the state self-corrects. hasSlottedElement checks this.children
  // directly — mirroring dc-table.js's/dc-empty-state.js's identical
  // need — so the first render is already correct, and recomputed fresh
  // (not just set true when found) so a disconnect/reconnect cycle with
  // content since removed doesn't leave a stale true from a prior
  // connection.
  connectedCallback() {
    super.connectedCallback();
    this._recomputeSlotState();
  }

  _recomputeSlotState() {
    this._hasTitle = hasSlottedElement(this, 'title');
    this._hasActions = hasSlottedElement(this, 'actions');
  }

  // Bound once (arrow class field) so both `@slotchange` bindings below
  // reuse one function identity across renders instead of Lit rebinding a
  // fresh closure on every unrelated re-render — same rationale as
  // dc-table.js's `_onSlotChange`.
  _onSlotChange = () => this._recomputeSlotState();

  render() {
    const showHeader = !!this.heading || this._hasTitle || this._hasActions;
    return html`
      <div class="header" ?hidden=${!showHeader}>
        <slot name="title" @slotchange=${this._onSlotChange}>
          ${this.heading ? html`<h2>${this.heading}</h2>` : ''}
        </slot>
        <div class="actions"><slot name="actions" @slotchange=${this._onSlotChange}></slot></div>
      </div>
      <div class="body">
        <slot></slot>
      </div>
    `;
  }
}

customElements.define('dc-card', DcCard);
