import { LitElement, html, css } from 'https://esm.sh/lit@3';

// <dc-status-badge> — Stage 1 primitive (#93): a colored status pill,
// generalizing the `<span class="badge badge-${status}">` pattern repeated
// across dc-task-list.js and dc-run-detail.js.
//
// Because this component uses Shadow DOM it can't rely on global.css's
// `.badge`/`.badge-*` rules cascading in (Shadow DOM blocks style
// *inheritance* of rules, though custom-property *values* still cross the
// boundary) — so the palette below is copied from global.css's badge
// section and re-expressed as `css` referencing the same theme.css tokens,
// keeping both light/dark themes in sync. If global.css's badge colors
// change, update both.
//
// Props:
//   status  — status string, e.g. 'success' | 'failure' | 'running' |
//             'crashlooping' | 'cancelled' | 'manual' | 'suspended' |
//             'resumed'. Unknown/empty values render the neutral style.
//
// No `variant` alias for `status`: a second name for the same prop, with
// precedence rules to resolve the ambiguity it creates, isn't worth adding
// to a component with zero consumers yet — the cheap moment not to have
// that API is before anything depends on it. Adding an alias later is
// never a breaking change; removing one is.
//
// Exported so consumers that need the exact known-status list (e.g. the
// demo page's exhaustive example) can reuse it instead of hand-duplicating
// it and drifting out of sync when a status is added here.
export const KNOWN_STATUSES = new Set([
  'success', 'failure', 'running', 'crashlooping',
  'cancelled', 'manual', 'suspended', 'resumed',
]);

class DcStatusBadge extends LitElement {
  static properties = {
    status: { type: String },
  };

  static styles = css`
    :host { display: inline-block; }
    .badge {
      display: inline-block;
      padding: .2em .6em;
      border-radius: var(--dicode-radius-sm);
      font-size: var(--dicode-text-xs);
      font-weight: var(--dicode-font-semibold);
      background: var(--dicode-card-bg);
      color: var(--dicode-muted);
      border: var(--dicode-border-width) solid var(--dicode-border);
    }
    /* Tints are mixed from the live token rather than written as a fixed
       rgba() of its dark-theme value: the green/red/yellow tokens all change
       under [data-theme="light"], and a baked-in tint would keep tinting
       with a hue the active theme no longer uses. */
    .badge-success {
      background: color-mix(in srgb, var(--dicode-green) 15%, transparent);
      color: var(--dicode-green);
      border-color: color-mix(in srgb, var(--dicode-green) 30%, transparent);
    }
    .badge-failure {
      background: color-mix(in srgb, var(--dicode-red) 15%, transparent);
      color: var(--dicode-red);
      border-color: color-mix(in srgb, var(--dicode-red) 30%, transparent);
    }
    .badge-running {
      background: color-mix(in srgb, var(--dicode-yellow) 15%, transparent);
      color: var(--dicode-yellow);
      border-color: color-mix(in srgb, var(--dicode-yellow) 30%, transparent);
    }
    /* crashlooping (#458): a daemon stuck in a spawn/crash/backoff loop —
       stronger red than badge-failure so it stands out. */
    .badge-crashlooping {
      background: color-mix(in srgb, var(--dicode-red) 28%, transparent);
      color: var(--dicode-red);
      border-color: color-mix(in srgb, var(--dicode-red) 40%, transparent);
    }
    .badge-cancelled {
      background: var(--dicode-card-bg);
      color: var(--dicode-muted);
      border-color: var(--dicode-border);
    }
    .badge-manual {
      background: var(--dicode-card-bg);
      color: var(--dicode-muted);
      border-color: var(--dicode-border);
    }
    /* suspended (#95): a run paused waiting on user input — blue "waiting on you". */
    .badge-suspended {
      background: var(--dicode-blue-tint);
      color: var(--dicode-sky);
      border-color: var(--dicode-blue-tint-strong);
    }
    .badge-resumed {
      background: var(--dicode-card-bg);
      color: var(--dicode-muted);
      border-color: var(--dicode-border);
    }
  `;

  constructor() {
    super();
    this.status = '';
  }

  render() {
    const v = this.status || '';
    const cls = KNOWN_STATUSES.has(v) ? `badge-${v}` : '';
    return html`<span class="badge ${cls}">${v || '—'}</span>`;
  }
}

customElements.define('dc-status-badge', DcStatusBadge);
