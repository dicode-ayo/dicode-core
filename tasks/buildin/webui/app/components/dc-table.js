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
//
// Shell is <div>s with CSS table display roles, NOT literal <table>/<thead>/
// <tbody> tags. The HTML parser only recognizes <slot> as valid content
// inside <table>-family elements when parsing *starts* already inside that
// table context; here it doesn't (this is a plain shadow-root template), so
// a literal `<table><thead><slot ...></slot></thead>...` gets its <slot>s
// foster-parented — silently relocated to just before the <table>, leaving
// <thead>/<tbody> permanently empty and the table itself zero-height.
// `display: table` on a <div> produces the identical CSS table layout
// (browsers key table layout off computed `display`, not tag names) without
// tripping the parser's table-insertion-mode special-casing, since a <div>
// never switches the parser into that mode in the first place. `role`
// attributes restore the semantics the swapped tags would otherwise carry.
class DcTable extends LitElement {
  static properties = {
    loading: { type: Boolean },
    emptyMessage: { type: String },
    emptyIcon: { type: String },
    _hasRows: { state: true },
    _hasHead: { state: true },
  };

  static styles = css`
    :host { display: block; }
    .table {
      display: table;
      width: 100%;
      border-collapse: collapse;
      background: var(--card-bg, rgba(255, 255, 255, .04));
      border: 1px solid var(--border, rgba(160, 196, 255, .15));
      border-radius: var(--radius-md, 10px);
      overflow: hidden;
    }
    .thead { display: table-header-group; }
    .tbody { display: table-row-group; }
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
    this._hasHead = true;
  }

  connectedCallback() {
    super.connectedCallback();
    this._recomputeHasRows();
  }

  // Bound once (arrow class field, not a prototype method) so every
  // `@slotchange=${this._onSlotChange}` binding below reuses the same
  // function identity — render() runs on every reactive-property change,
  // including unrelated ones like `loading` toggling, and a fresh
  // `() => this._recomputeHasRows()` closure per occurrence per render
  // would mean Lit re-diffs/rebinds up to 3 new listeners each time
  // instead of recognizing the same handler across renders.
  _onSlotChange = () => this._recomputeHasRows();

  // Body rows are any light-DOM children not assigned to the "head" slot;
  // the head row is any child that is. Checked directly against
  // this.children (rather than relying solely on slotchange, which isn't
  // guaranteed to fire for an initially-empty assignment) so the very
  // first render is already correct. Tracked separately because a head
  // row can be present with zero body rows (a consumer appending its
  // header before any data arrives) — see the no-body-rows render branch.
  _recomputeHasRows() {
    const children = [...this.children];
    this._hasRows = children.some(el => el.getAttribute('slot') !== 'head');
    this._hasHead = children.some(el => el.getAttribute('slot') === 'head');
  }

  render() {
    if (this.loading) return html`<div class="loading">Loading…</div>`;

    if (!this._hasRows) {
      // A head row can be present even with zero body rows (a consumer
      // appends its header before any data arrives) — when it is, still
      // route it through the table shell so it renders as a styled header
      // instead of a bare, unbordered <tr>. When there's no head either
      // (the ordinary fully-empty case), skip the shell entirely — an
      // empty bordered box floating above the empty-state message would
      // be its own small visual bug. The default slot has no visible
      // content in this branch (there are no body rows by definition) but
      // stays mounted so its slotchange listener still fires when rows
      // are appended later.
      return html`
        ${this._hasHead ? html`
          <div class="table" role="table">
            <div class="thead" role="rowgroup"><slot name="head" @slotchange=${this._onSlotChange}></slot></div>
          </div>
        ` : html`<slot name="head" @slotchange=${this._onSlotChange}></slot>`}
        <slot @slotchange=${this._onSlotChange}></slot>
        <dc-empty-state icon=${this.emptyIcon} message=${this.emptyMessage}></dc-empty-state>
      `;
    }

    return html`
      <div class="table" role="table">
        <div class="thead" role="rowgroup"><slot name="head" @slotchange=${this._onSlotChange}></slot></div>
        <div class="tbody" role="rowgroup"><slot @slotchange=${this._onSlotChange}></slot></div>
      </div>
    `;
  }
}

customElements.define('dc-table', DcTable);
