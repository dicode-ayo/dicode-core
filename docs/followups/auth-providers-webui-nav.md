# Follow-up: generic "task contributes a webui sub-page"

**Status:** deferred from
[`docs/superpowers/specs/2026-04-26-auth-providers-design.md`](../superpowers/specs/2026-04-26-auth-providers-design.md)
to keep that PR focused.

**Open this as a GitHub issue and link the auth-providers PR.**

## Background

Today, any task that ships an `index.html` is auto-served as a SPA at
its webhook path with the dicode.js SDK injected
([`pkg/trigger/engine.go:780-869`](../../pkg/trigger/engine.go)). This
means `tasks/buildin/auth-providers/` already works as a self-contained
SPA at `/hooks/auth-providers`.

What's missing: there is no way for the existing webui SPA at
`/hooks/webui` to *discover* such tasks and surface them as first-class
nav entries. Today users reach the auth-providers page by drilling into
the Tasks list and clicking "open webhook UI" on the task-detail page.

## Proposal

Add a `Webui *WebuiConfig` field to `Spec` ([`pkg/task/spec.go`](../../pkg/task/spec.go))
that any task may set:

```go
type WebuiConfig struct {
    Nav *WebuiNav `yaml:"nav,omitempty" json:"nav,omitempty"`
}

type WebuiNav struct {
    Label string `yaml:"label" json:"label"`     // e.g. "Providers"
    Order int    `yaml:"order,omitempty" json:"order,omitempty"`  // optional ordering hint
    Icon  string `yaml:"icon,omitempty" json:"icon,omitempty"`    // optional icon name
}
```

Webui SPA changes:

1. `GET /api/tasks` already returns each task's full spec; webui's
   nav-rendering code (`tasks/buildin/webui/app/components/dc-nav.js`,
   new) filters for tasks whose `Webui.Nav.Label` is set.
2. Each filtered task adds a nav entry to the existing header alongside
   `Tasks | Sources | Config | Secrets | …`, ordered by
   `Webui.Nav.Order` (stable ascending; ties broken by task ID).
3. Clicking the entry routes to a webui page that renders an
   `<iframe src="/hooks/<webhook>">` inside the existing chrome.
4. Theme propagation: the webui shell already sets
   `<html data-theme="…">`; the iframe document inherits via its own
   `data-theme` boot script (already present in the webui SPA pattern;
   the auth-providers SPA must do the same).

## First adopters

Once shipped:
- `tasks/buildin/auth-providers/task.yaml` adds
  `webui: { nav: { label: "Providers", order: 50 } }`.
- `tasks/buildin/webui/task.yaml` adds the same metadata for symmetry
  (with `order: 0`), making it self-describing rather than special-cased
  in routing.

## Security considerations

- Iframe sandbox: same-origin (every webhook is served from the daemon),
  so `sandbox` cannot use `allow-same-origin=false`. Trust boundary
  inside the daemon is the secrets manager + permissions, not the
  iframe.
- The webui must not trust arbitrary task-supplied `label` / `icon`
  values without sanitisation — render as text, never `innerHTML`.
- `Order` is an integer; clamp to a sane range (`-1000..1000`).

## Open design questions for the issue

- Should built-in nav entries (Tasks, Sources, …) be representable in
  the same model? Probably yes — the webui task itself adopts the
  metadata, and the static header turns into the dynamic-render path.
- Should each task's nav entry appear only when its trigger.auth: true
  matches the user's session, or always? Default: always; clicking
  routes through the existing auth-redirect.
- Do we want a nav-section grouping mechanism (e.g.
  `Webui.Nav.Section: "tools"`) or stay flat? Stay flat for now.
