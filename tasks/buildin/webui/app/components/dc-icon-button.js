import { LitElement, html, css } from 'https://esm.sh/lit@3';

// <dc-icon-button> — Stage 1 primitive (#93): a real <button> wrapping a
// slotted icon, with an `aria-label` sourced from a required `label` prop
// instead of relying on the icon being self-describing. Generalizes the
// pattern in dc-task-list.js's `.toggle-btn` and dc-theme-toggle.js's
// icon `<button>`.
//
// Slots:
//   (default) — icon content, typically an inline <svg>
//
// Props:
//   label    — accessible name (required); also used as the title tooltip
//   variant  — 'default' | 'danger' — tints hover/focus color
//   disabled — standard boolean
class DcIconButton extends LitElement {
  static properties = {
    label: { type: String },
    variant: { type: String, reflect: true },
    disabled: { type: Boolean, reflect: true },
  };

  static styles = css`
    :host { display: inline-block; }
    button {
      display: flex;
      align-items: center;
      justify-content: center;
      width: 32px;
      height: 32px;
      padding: 0;
      background: transparent;
      border: 1px solid var(--border, rgba(160, 196, 255, .15));
      border-radius: var(--radius-md, 10px);
      color: var(--muted, #8b93a8);
      cursor: pointer;
      font: inherit;
      transition: background .2s var(--ease, ease), border-color .2s var(--ease, ease), color .2s var(--ease, ease);
    }
    button:hover {
      background: var(--card-bg, rgba(255, 255, 255, .04));
      border-color: var(--sky, #a0c4ff);
      color: var(--sky, #a0c4ff);
    }
    button:focus-visible {
      outline: 2px solid var(--sky, #a0c4ff);
      outline-offset: 2px;
    }
    :host([variant='danger']) button:hover,
    :host([variant='danger']) button:focus-visible {
      border-color: var(--red, #f38ba8);
      color: var(--red, #f38ba8);
    }
    :host([variant='danger']) button:focus-visible {
      outline-color: var(--red, #f38ba8);
    }
    button:disabled { opacity: .5; cursor: not-allowed; }
    ::slotted(svg) { width: 18px; height: 18px; }
  `;

  constructor() {
    super();
    this.label = '';
    this.variant = 'default';
    this.disabled = false;
  }

  render() {
    return html`
      <button type="button" aria-label=${this.label} title=${this.label} ?disabled=${this.disabled}>
        <slot></slot>
      </button>
    `;
  }
}

customElements.define('dc-icon-button', DcIconButton);
