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
// Known issue, tracked in #710 (not fixed here to avoid this file and
// global.css drifting apart on the fix itself): the tint rgba() literals
// below are baked from the dark theme's --green/--red/--yellow values, so
// in light mode `color` correctly follows the live (light) token while
// `background`/`border-color` keep the dark hue. Inherited faithfully from
// global.css, which has the identical bug.
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
      border-radius: var(--radius-sm);
      font-size: var(--text-xs);
      font-weight: var(--font-semibold);
      background: var(--card-bg);
      color: var(--muted);
      border: 1px solid var(--border);
    }
    .badge-success {
      background: rgba(166, 227, 161, .15);
      color: var(--green);
      border-color: rgba(166, 227, 161, .3);
    }
    .badge-failure {
      background: rgba(243, 139, 168, .15);
      color: var(--red);
      border-color: rgba(243, 139, 168, .3);
    }
    .badge-running {
      background: rgba(249, 226, 175, .15);
      color: var(--yellow);
      border-color: rgba(249, 226, 175, .3);
    }
    /* crashlooping (#458): a daemon stuck in a spawn/crash/backoff loop —
       stronger red than badge-failure so it stands out. */
    .badge-crashlooping {
      background: rgba(243, 139, 168, .28);
      color: var(--red);
      border-color: rgba(243, 139, 168, .4);
    }
    .badge-cancelled {
      background: var(--card-bg);
      color: var(--muted);
      border-color: var(--border);
    }
    .badge-manual {
      background: var(--card-bg);
      color: var(--muted);
      border-color: var(--border);
    }
    /* suspended (#95): a run paused waiting on user input — blue "waiting on you". */
    .badge-suspended {
      background: var(--blue-tint);
      color: var(--sky);
      border-color: var(--blue-tint-strong);
    }
    .badge-resumed {
      background: var(--card-bg);
      color: var(--muted);
      border-color: var(--border);
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
