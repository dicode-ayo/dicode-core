import { LitElement, html, css } from 'https://esm.sh/lit@3';
import { hasSlottedElement } from '../lib/slot-utils.js';

// <dc-empty-state> — Stage 1 primitive (#93): the centered "nothing here"
// placeholder duplicated inline in dc-task-list.js
// (`<div class="card" style="text-align:center;...">No tasks found...</div>`)
// and similar spots.
//
// Slots:
//   icon      — optional custom icon/graphic; falls back to the `icon` prop
//               (an emoji/text glyph)
//   (default) — optional call-to-action content (e.g. a <button>) rendered
//               below the message
//
// Props:
//   icon    — emoji/text glyph shown above the message when no `icon` slot
//             content is provided
//   message — the empty-state text
class DcEmptyState extends LitElement {
  static properties = {
    icon: { type: String },
    message: { type: String },
    _hasCta: { state: true },
  };

  static styles = css`
    :host {
      display: block;
      text-align: center;
      color: var(--muted);
      padding: var(--space-xl);
    }
    .icon {
      font-size: var(--text-2xl);
      line-height: 1;
      margin-bottom: var(--space-sm);
    }
    .message {
      font-size: var(--text-base);
    }
    /* Gap only when the CTA slot has content, so icon/message-only
       instances reserve no dead space. The presence test cannot be
       ".cta:has(*)": :has() matches against this shadow tree, where the
       <slot> element is unconditionally a child of .cta whether or not
       anything is assigned to it, so that selector always matches. */
    .cta.has-content {
      margin-top: var(--space-md);
    }
  `;

  constructor() {
    super();
    this.icon = '';
    this.message = '';
    this._hasCta = false;
  }

  // Synchronous census, not slotchange-only, so a CTA present from the
  // first connection never renders gap-less for a frame before
  // self-correcting. hasSlottedElement counts element children only:
  // `<slot>.assignedNodes()` would also count the whitespace text node
  // that ordinary multi-line `<dc-empty-state>` markup leaves behind,
  // turning the gap on for instances with no CTA at all.
  connectedCallback() {
    super.connectedCallback();
    this._hasCta = hasSlottedElement(this);
  }

  _onCtaSlotChange = () => { this._hasCta = hasSlottedElement(this); };

  render() {
    return html`
      <slot name="icon">${this.icon ? html`<div class="icon" aria-hidden="true">${this.icon}</div>` : ''}</slot>
      <div class="message">${this.message}</div>
      <div class="cta ${this._hasCta ? 'has-content' : ''}"><slot @slotchange=${this._onCtaSlotChange}></slot></div>
    `;
  }
}

customElements.define('dc-empty-state', DcEmptyState);
