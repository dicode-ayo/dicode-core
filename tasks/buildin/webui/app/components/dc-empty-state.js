import { LitElement, html, css } from 'https://esm.sh/lit@3';

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
  };

  static styles = css`
    :host {
      display: block;
      text-align: center;
      color: var(--dicode-muted);
      padding: var(--dicode-space-xl);
    }
    .icon {
      font-size: var(--dicode-text-2xl);
      line-height: 1;
      margin-bottom: var(--dicode-space-sm);
    }
    .message {
      font-size: var(--dicode-text-base);
    }
    /* Only add the gap above the CTA slot when it actually has content —
       avoids reserving dead space for icon/message-only instances.
       ::slotted(*) only matches an element that is actually assigned to
       the slot — a whitespace text node between this component's open
       and close tags (ordinary multi-line markup with no real CTA
       element) never matches — so this needs no JS census, no
       connectedCallback, and no slotchange listener; the browser's own
       slot-assignment tracking does the work. (An earlier version of
       this component tried ".cta:has(*)" for the same goal — wrong,
       because :has() matches this shadow tree, where the <slot> element
       itself is unconditionally present regardless of assignment, not
       the flattened/slotted tree.) Caveat: the margin lands on each
       slotted element individually rather than once on a wrapper, so two
       stacked CTA elements would each carry it — acceptable for this
       component's one-CTA-element contract. */
    slot:not([name])::slotted(*) {
      display: inline-block;
      margin-block-start: var(--dicode-space-md);
    }
  `;

  constructor() {
    super();
    this.icon = '';
    this.message = '';
  }

  render() {
    return html`
      <slot name="icon">${this.icon ? html`<div class="icon" aria-hidden="true">${this.icon}</div>` : ''}</slot>
      <div class="message">${this.message}</div>
      <slot></slot>
    `;
  }
}

customElements.define('dc-empty-state', DcEmptyState);
