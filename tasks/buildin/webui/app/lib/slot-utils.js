// hasSlottedElement checks a host's *current* light-DOM children (not
// `<slot>.assignedNodes()`) for an Element assigned to the given slot —
// the default slot when `slotName` is omitted/empty, a named slot
// otherwise.
//
// Element children only, deliberately: this is the shared "does this slot
// have real content" check used by dc-card.js/dc-table.js/dc-empty-state.js
// to decide whether to show a header row, a table shell, or a CTA gap.
// `assignedNodes()` also returns whitespace text nodes (e.g. the newline
// between `<dc-empty-state>` and `</dc-empty-state>` in ordinary
// multi-line markup with no real CTA element), which would make these
// components' "has content" checks flip true for elements with no visible
// content at all. `this.children` is unaffected by that, and — checked
// directly rather than only on `slotchange` — lets a component's very
// first render already be correct instead of momentarily showing the
// no-content state and self-correcting one render later.
export function hasSlottedElement(host, slotName = '') {
  for (const el of host.children) {
    if ((el.getAttribute('slot') || '') === slotName) return true;
  }
  return false;
}
