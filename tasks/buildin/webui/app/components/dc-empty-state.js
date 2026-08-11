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
      color: var(--muted, #8b93a8);
      padding: var(--space-xl, 2rem);
    }
    .icon {
      font-size: 1.8rem;
      line-height: 1;
      margin-bottom: var(--space-sm, .5rem);
    }
    .message {
      font-size: var(--text-base, .9rem);
    }
    .cta {
      margin-top: 0;
    }
    /* Only add the gap above the CTA slot when it actually has content —
       avoids reserving dead space for icon/message-only instances. */
    .cta:has(*) {
      margin-top: var(--space-md, 1rem);
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
      <div class="cta"><slot></slot></div>
    `;
  }
}

customElements.define('dc-empty-state', DcEmptyState);
