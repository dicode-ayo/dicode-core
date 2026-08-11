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
//   status  — status/variant string, e.g. 'success' | 'failure' | 'running' |
//             'crashlooping' | 'cancelled' | 'manual' | 'suspended' |
//             'resumed'. Unknown/empty values render the neutral style.
//   variant — alias for `status` (either may be set; `status` wins if both
//             are given)
// Exported so consumers that need the exact known-status list (e.g. the
// demo page's exhaustive example) can reuse it instead of hand-duplicating
// it and drifting out of sync when a status is added here.
export const KNOWN_VARIANTS = new Set([
  'success', 'failure', 'running', 'crashlooping',
  'cancelled', 'manual', 'suspended', 'resumed',
]);

class DcStatusBadge extends LitElement {
  static properties = {
    status: { type: String },
    variant: { type: String },
  };

  static styles = css`
    :host { display: inline-block; }
    .badge {
      display: inline-block;
      padding: .2em .6em;
      border-radius: var(--radius-sm, 6px);
      font-size: var(--text-xs, .72rem);
      font-weight: var(--font-semibold, 600);
      background: var(--card-bg, rgba(255, 255, 255, .04));
      color: var(--muted, #8b93a8);
      border: 1px solid var(--border, rgba(160, 196, 255, .15));
    }
    .badge-success {
      background: rgba(166, 227, 161, .15);
      color: var(--green, #a6e3a1);
      border-color: rgba(166, 227, 161, .3);
    }
    .badge-failure {
      background: rgba(243, 139, 168, .15);
      color: var(--red, #f38ba8);
      border-color: rgba(243, 139, 168, .3);
    }
    .badge-running {
      background: rgba(249, 226, 175, .15);
      color: var(--yellow, #f9e2af);
      border-color: rgba(249, 226, 175, .3);
    }
    /* crashlooping (#458): a daemon stuck in a spawn/crash/backoff loop —
       stronger red than badge-failure so it stands out. */
    .badge-crashlooping {
      background: rgba(243, 139, 168, .28);
      color: var(--red, #f38ba8);
      border-color: rgba(243, 139, 168, .4);
    }
    .badge-cancelled {
      background: var(--card-bg, rgba(255, 255, 255, .04));
      color: var(--muted, #8b93a8);
      border-color: var(--border, rgba(160, 196, 255, .15));
    }
    .badge-manual {
      background: var(--card-bg, rgba(255, 255, 255, .04));
      color: var(--muted, #8b93a8);
      border-color: var(--border, rgba(160, 196, 255, .15));
    }
    /* suspended (#95): a run paused waiting on user input — blue "waiting on you". */
    .badge-suspended {
      background: var(--blue-tint, rgba(13, 110, 253, .12));
      color: var(--sky, #a0c4ff);
      border-color: var(--blue-tint-strong, rgba(13, 110, 253, .18));
    }
    .badge-resumed {
      background: var(--card-bg, rgba(255, 255, 255, .04));
      color: var(--muted, #8b93a8);
      border-color: var(--border, rgba(160, 196, 255, .15));
    }
  `;

  constructor() {
    super();
    this.status = '';
    this.variant = '';
  }

  render() {
    const v = this.status || this.variant || '';
    const cls = KNOWN_VARIANTS.has(v) ? `badge-${v}` : '';
    return html`<span class="badge ${cls}">${v || '—'}</span>`;
  }
}

customElements.define('dc-status-badge', DcStatusBadge);
