import { LitElement, html, css } from 'https://esm.sh/lit@3';
import './dc-empty-state.js';

// <dc-table> — Stage 1 primitive (#93): a bordered table shell around
// slotted <tr> rows, generalizing the `<table>...<tbody>${rows.map(...)}` +
// "no rows" card-message pattern in dc-task-list.js.
//
// Slots:
//   head      — a single <tr> (with <th> cells) rendered inside <thead>
//   (default) — <tr> rows rendered inside <tbody>
//
// Props:
//   loading      — show a loading placeholder instead of slotted content
//   emptyMessage — message passed to the internal <dc-empty-state> when
//                  there are no slotted body rows and `loading` is false
//   emptyIcon    — icon glyph passed to <dc-empty-state icon>
//
// Note: slotted <tr>/<td>/<th> elements remain in the *light* DOM — CSS
// projection through a <slot> only changes where a node paints, not which
// document tree selectors match it against — so the app's global.css table
// rules (`th, td { ... }`) keep styling their contents without this
// component needing to duplicate them.
class DcTable extends LitElement {
  static properties = {
    loading: { type: Boolean },
    emptyMessage: { type: String },
    emptyIcon: { type: String },
    _hasRows: { state: true },
  };

  static styles = css`
    :host { display: block; }
    table {
      width: 100%;
      border-collapse: collapse;
      background: var(--card-bg, rgba(255, 255, 255, .04));
      border: 1px solid var(--border, rgba(160, 196, 255, .15));
      border-radius: var(--radius-md, 10px);
      overflow: hidden;
    }
    .loading {
      padding: var(--space-lg, 1.5rem);
      text-align: center;
      color: var(--muted, #8b93a8);
      font-size: var(--text-sm, .82rem);
    }
  `;

  constructor() {
    super();
    this.loading = false;
    this.emptyMessage = 'Nothing to show.';
    this.emptyIcon = '';
    // Assume non-empty until the initial light-DOM check runs in
    // connectedCallback, so a table that *does* have rows never flashes
    // the empty state on first paint.
    this._hasRows = true;
  }

  connectedCallback() {
    super.connectedCallback();
    this._recomputeHasRows();
  }

  // Body rows are any light-DOM children not assigned to the "head" slot.
  // Checked directly against this.children (rather than relying solely on
  // slotchange, which isn't guaranteed to fire for an initially-empty
  // assignment) so the very first render is already correct.
  _recomputeHasRows() {
    this._hasRows = [...this.children].some(el => el.getAttribute('slot') !== 'head');
  }

  render() {
    if (this.loading) return html`<div class="loading">Loading…</div>`;

    if (!this._hasRows) {
      return html`
        <slot name="head" @slotchange=${() => this._recomputeHasRows()}></slot>
        <slot @slotchange=${() => this._recomputeHasRows()}></slot>
        <dc-empty-state icon=${this.emptyIcon} message=${this.emptyMessage}></dc-empty-state>
      `;
    }

    return html`
      <table>
        <thead><slot name="head" @slotchange=${() => this._recomputeHasRows()}></slot></thead>
        <tbody><slot @slotchange=${() => this._recomputeHasRows()}></slot></tbody>
      </table>
    `;
  }
}

customElements.define('dc-table', DcTable);
