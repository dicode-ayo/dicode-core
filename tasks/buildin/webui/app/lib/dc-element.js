import { LitElement } from 'https://esm.sh/lit@3';

// DcElement — Stage 2 (#93) plumbing: a thin base class for the app's
// light-DOM page components. It formalizes the `_loading`/`_error` +
// try/catch-around-a-load-call pattern already duplicated ad hoc across
// dc-task-list.js's `_load()`, dc-run-detail.js's `_load()`, etc.
//
// This is *not* yet adopted by any existing component (that's Stage 3) — it
// ships now so new components can opt in immediately and existing ones can
// migrate incrementally without a big-bang rewrite.
//
// Usage:
//
//   import { DcElement } from '../lib/dc-element.js';
//   import { get } from '../lib/api.js';
//
//   class DcThing extends DcElement {
//     static properties = { ...DcElement.properties, _things: { state: true } };
//     connectedCallback() {
//       super.connectedCallback();
//       this._load();
//     }
//     async _load() {
//       this._things = await this._fetch(() => get('/api/things'));
//     }
//     render() {
//       if (this._loading) return html`<div class="meta">Loading…</div>`;
//       if (this._error) return html`<p style="color:red">Error: ${this._error}</p>`;
//       ...
//     }
//   }
export class DcElement extends LitElement {
  // Matches the rest of the app's `dc-*` components: styled entirely via
  // the page's global.css rather than Shadow DOM encapsulation. Shadow DOM
  // is reserved for the reusable dc-card/dc-table/etc. primitives — see
  // docs/concepts/webui-components.md.
  createRenderRoot() {
    return this;
  }

  static properties = {
    _loading: { state: true },
    _error: { state: true },
  };

  constructor() {
    super();
    this._loading = false;
    this._error = null;
  }

  // _fetch wraps a single async call (typically one of lib/api.js's
  // get/post/patch/del, or a Promise-returning function), toggling
  // `_loading` around it and capturing any thrown error into `_error` in
  // one shot. Accepts either a Promise or a zero-arg function returning one
  // — the function form lets callers defer starting the request until
  // `_loading` has actually been set to true.
  //
  // Returns the resolved value on success, or `undefined` on failure —
  // callers that need to distinguish "no data yet" from "failed" should
  // check `this._error` after awaiting.
  async _fetch(promiseOrFn) {
    this._loading = true;
    this._error = null;
    try {
      return typeof promiseOrFn === 'function' ? await promiseOrFn() : await promiseOrFn;
    } catch (e) {
      this._error = e?.message || String(e);
      return undefined;
    } finally {
      this._loading = false;
    }
  }
}
