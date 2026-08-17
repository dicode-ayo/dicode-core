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
      inline-size: 2rem;
      block-size: 2rem;
      padding: 0;
      background: transparent;
      border: 1px solid var(--border);
      border-radius: var(--radius-md);
      color: var(--muted);
      cursor: pointer;
      font: inherit;
      transition: background .2s var(--ease), border-color .2s var(--ease), color .2s var(--ease);
    }
    button:hover {
      background: var(--card-bg);
      border-color: var(--sky);
      color: var(--sky);
    }
    button:focus-visible {
      outline: 2px solid var(--sky);
      outline-offset: 2px;
    }
    :host([variant='danger']) button:hover,
    :host([variant='danger']) button:focus-visible {
      border-color: var(--red);
      color: var(--red);
    }
    :host([variant='danger']) button:focus-visible {
      outline-color: var(--red);
    }
    button:disabled { opacity: .5; cursor: not-allowed; }
    ::slotted(svg) { inline-size: 1.125rem; block-size: 1.125rem; }
  `;

  constructor() {
    super();
    this.label = '';
    // No default assigned: `variant` reflects, and with @lit/reactive-element's
    // actual reflection timing, a constructor-assigned default would stamp
    // `variant="default"` onto every instance's host attribute on first
    // update. Nothing keys off that value — the stylesheet only knows
    // `:host([variant='danger'])`, and any other/missing value already gets
    // the base look — so the sentinel attribute would buy nothing while
    // becoming an undocumented part of the DOM consumers could start
    // depending on. Leaving it unset means no attribute is written until a
    // consumer actually sets `variant="danger"`.
    this.disabled = false;
  }

  // `label` is documented as required (it's the button's only accessible
  // name — an icon alone isn't one), but nothing enforced that: the
  // constructor's `''` default plus `aria-label=${this.label}` silently
  // renders `aria-label=""`, which gives an icon-only button no accessible
  // name at all — and the empty (not missing) attribute hides the gap from
  // audit tooling that would otherwise flag a nameless button. Surfacing it
  // loudly in dev rather than only in an accessibility audit.
  willUpdate(changedProperties) {
    super.willUpdate(changedProperties);
    if (changedProperties.has('label') && !this.label) {
      console.warn('<dc-icon-button> requires a non-empty `label`; an icon-only button has no accessible name without one.', this);
    }
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
