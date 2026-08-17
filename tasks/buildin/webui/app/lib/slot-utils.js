// hasSlottedElement checks a host's *current* light-DOM children (not
// `<slot>.assignedNodes()`) for an Element assigned to the given slot —
// the default slot when `slotName` is omitted/empty, a named slot
// otherwise.
//
// Element children only: `assignedNodes()` also returns whitespace text
// nodes — the newline in ordinary multi-line markup with no real slotted
// element — which would flip a "has content" check true for a host with
// nothing visible in that slot. Checking `host.children` directly, rather
// than only on `slotchange`, also lets a component's first render be
// correct instead of showing the no-content state and self-correcting one
// render later.
//
// Use this where the answer toggles state on an ancestor (showing a header
// box around slotted content). Where the styling targets the slotted
// elements themselves, `::slotted(*)` does the same job in CSS alone and
// never matches whitespace either — see dc-empty-state.js.
export function hasSlottedElement(host, slotName = '') {
  for (const el of host.children) {
    if ((el.getAttribute('slot') || '') === slotName) return true;
  }
  return false;
}
