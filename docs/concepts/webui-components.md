# WebUI frontend components

Internal contributor documentation for `tasks/buildin/webui/app/` — the plain
Lit 3 SPA served at `/hooks/webui` (see [Web UI & API](webui-api.md) for the
pages/routes/REST surface). This doc covers the component-level
architecture: the light-DOM/Shadow-DOM split, the Stage 1 primitives
(#93), and the `DcElement` base class.

This is **Stage 1 + Stage 2 of issue #93**. Stages 3-4 (migrating the
existing page components onto `DcElement`, and consolidating the
API/routing layer) are tracked as follow-up and not covered here.

---

## Two kinds of component

The app has always been plain ESM Lit components loaded via `esm.sh` — no
bundler, no build step (see [Web UI & API — Frontend](webui-api.md#frontend)).
As of #93 there are two distinct flavors:

| | Light-DOM app components | Shadow-DOM primitives |
| --- | --- | --- |
| Examples | `dc-task-list`, `dc-task-detail`, `dc-run-detail`, `dc-nav`, `dc-ui-kit-demo` | `dc-card`, `dc-page-header`, `dc-empty-state`, `dc-icon-button`, `dc-status-badge`, `dc-table` |
| `createRenderRoot()` | overridden to `return this;` | default (real shadow root) |
| Styling | shares `global.css`/`theme.css` loaded once by `index.html`; per-component `<style>` tags scoped by tag-name selector when needed | each has its own `static styles = css\`...\`` block; no reliance on `global.css` cascading in |
| Role | one per page/route; owns data fetching, routing, app-specific behavior | reusable, presentation-only building blocks composed *by* the app components |

Existing page components predate this split and intentionally stay
light-DOM/unmigrated in this pass — see `CLAUDE.md`'s note on the narrow
scope of this change. New reusable pieces (the six primitives below) use
Shadow DOM: real encapsulation is worth it once something is meant to be
dropped into multiple pages without its internals leaking into (or
colliding with) `global.css`.

### Theming across the shadow boundary

Shadow DOM blocks style *rules* from crossing in, but CSS **custom property
values** still inherit through shadow boundaries (`:host` picks them up like
any other descendant). Every primitive's `css` styles reference the same
tokens defined in `theme.css` (`var(--space-md)`, `var(--card-bg)`,
`var(--blue)`, ...) with a hard-coded fallback matching the dark-theme
default, so:

- swapping `data-theme="light"`/`"dark"` on `<html>` re-tints primitives
  exactly like the rest of the app, with no extra wiring:
- a primitive dropped onto a page that doesn't happen to load `theme.css`
  still renders sensibly, via the fallback values.

Where a primitive's own palette needs to match `global.css` exactly (see
`dc-status-badge` below), the relevant rules are copied into the
component's `css` template rather than imported, since `@import` inside a
Shadow DOM `css` template would defeat the encapsulation and doesn't
resolve relative to `esm.sh`-loaded modules cleanly. If `global.css`'s
badge colors change, both places need updating — noted in both files.

---

## The six Stage 1 primitives

All under `tasks/buildin/webui/app/components/`. Each is self-contained
(imports only `lit` and, for `dc-table`, `dc-empty-state`).

### `<dc-card>`

Bordered surface (`.card`-equivalent), replacing ad hoc
`<div class="card" style="...">` blocks.

- Slots: `title` (header, falls back to a plain `<h2>` from `heading`),
  `actions` (right-aligned header controls), default (body)
- Props: `heading` (string), `pad` (`'md'` default | `'none'`)

### `<dc-page-header>`

The page-title-plus-controls row pattern (e.g. dc-task-list.js's `<h1>Tasks</h1>`
+ filter input).

- Slots: default (right-aligned actions)
- Props: `heading` (required), `subtitle` (optional, muted)

### `<dc-empty-state>`

Centered "nothing here" placeholder.

- Slots: `icon` (falls back to the `icon` prop glyph), default (optional CTA,
  e.g. a `<button>`)
- Props: `icon` (emoji/glyph), `message`

Composed internally by `<dc-table>` for its empty state.

### `<dc-icon-button>`

A real `<button>` around a slotted icon (typically inline `<svg>`), with a
required `label` prop supplying `aria-label`/`title` — the accessible name
never depends on the icon being self-describing.

- Slots: default (icon content)
- Props: `label` (required), `variant` (`'default'` | `'danger'`), `disabled`

### `<dc-status-badge>`

Colored status pill generalizing `<span class="badge badge-${status}">`.

- Props: `status` (or `variant` as an alias) — one of `success`, `failure`,
  `running`, `crashlooping`, `cancelled`, `manual`, `suspended`, `resumed`;
  any other value renders the neutral/default style.

Its palette is a copy of `global.css`'s `.badge`/`.badge-*` rules (see
[Theming across the shadow boundary](#theming-across-the-shadow-boundary)
above for why it's a copy, not an import).

### `<dc-table>`

Bordered table shell around slotted rows, generalizing the
`<table>...</table>` + "no tasks found" empty-card pattern in
`dc-task-list.js`.

- Slots: `head` (a single `<tr>`, rendered as the table's header row group),
  default (`<tr>` rows rendered as the body row group)
- Props: `loading` (shows a placeholder instead of rows), `emptyMessage` /
  `emptyIcon` (forwarded to the internal `<dc-empty-state>` when no rows are
  slotted)

`emptyMessage`/`emptyIcon` must be set as JS properties (`.emptyMessage=`)
rather than HTML attributes from a Lit template — Lit's default attribute
name for a property is just the lowercased property name (`emptymessage`),
not a kebab-cased one, so a literal `empty-message="..."` attribute would
not bind.

Slotted `<tr>`/`<td>`/`<th>` elements stay in the *light* DOM — projection
through a `<slot>` only changes where a node paints, not which document
tree it lives in for CSS-selector purposes — so the app's `global.css`
table rules keep styling their contents without `<dc-table>` needing to
duplicate them.

**Rows cannot be written as literal `<tr>` markup in a Lit template**, e.g.
`html\`<dc-table><tr>...</tr></dc-table>\``. The HTML parser only creates
`<tr>`/`<td>`/`<th>` elements while in one of its table-specific insertion
modes, entered only after a literal `<table>` tag has already been opened
in the *same* parse — `<dc-table>` doesn't count, so the parser stays in
its default mode the whole way through and silently drops the tag (a
`<tr>` start tag there is a parse error; the tag is ignored, though its
text content still leaks through as loose text). This is a general
HTML-parsing constraint, not specific to `<dc-table>`'s own shadow
template — it bites any element that isn't literally `<table>`. Build rows
with `document.createElement`/`appendChild` instead (bypasses HTML parsing
entirely) and attach them via Lit's `ref()` directive; see
`_populateTableRows` in `dc-ui-kit-demo.js` for the pattern. `<dc-table>`'s
own shadow-root shell is `<div>`s with CSS `display: table` roles rather
than literal `<table>`/`<thead>`/`<tbody>`, for the same underlying
reason: a literal `<table>` there would foster-parent its own `<slot>`
children right back out.

### Demo page

`components/dc-ui-kit-demo.js`, routed at `/ui-kit` (`/hooks/webui/ui-kit`
once served) by `app.js`. Not linked from the persistent nav — reachable by
direct URL like any other route. Exercises all six primitives with a
couple of realistic instances each, including a `<dc-table>` in both its
populated and empty states. Covered by
`tests/e2e/ui-kit-primitives.spec.ts` (Playwright's default locators pierce
open shadow roots, so the spec asserts directly against each primitive's
shadow-rendered output and projected/slotted content).

---

## `DcElement` (Stage 2)

`lib/dc-element.js` exports a small `DcElement extends LitElement` base
class formalizing the `_loading`/`_error` + try/catch-around-a-load-call
pattern already duplicated ad hoc in `dc-task-list.js`'s `_load()`,
`dc-run-detail.js`'s `_load()`, and others:

```js
class DcThing extends DcElement {
  static properties = { ...DcElement.properties, _things: { state: true } };
  connectedCallback() {
    super.connectedCallback();
    this._load();
  }
  async _load() {
    this._things = await this._fetch(() => get('/api/things'));
  }
}
```

`_fetch(promiseOrFn)` sets `_loading = true`, clears `_error`, awaits the
call, and on failure sets `_error` to the caught error's message — all in
one shot, returning the resolved value (or `undefined` on failure).

Like the app's other light-DOM components, `DcElement` overrides
`createRenderRoot()` to render into light DOM — it's plumbing for the
*app* components, not a primitive, so it deliberately does not use Shadow
DOM.

**No existing component has been migrated onto `DcElement` yet.** It ships
now, unused by the pre-#93 components, so new components can opt in
immediately; migrating `dc-task-list.js`/`dc-run-detail.js`/etc. onto it is
Stage 3 follow-up work, chosen to stay out of scope here since (per
`CLAUDE.md`) every other in-flight PR at the time of this change already
touches one of those files.
