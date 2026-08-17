import { LitElement, html, css } from 'https://esm.sh/lit@3';

// <dc-page-header> — Stage 1 primitive (#93): the page-title row pattern
// duplicated inline across dc-task-list.js (`<h1>Tasks</h1>` + filter input)
// and similar `<h1>`-plus-controls headers elsewhere.
//
// Slots:
//   (default) — right-aligned actions (buttons, search inputs, ...)
//
// Props:
//   heading  — page title (required)
//   subtitle — optional muted subtitle rendered under the heading
class DcPageHeader extends LitElement {
  static properties = {
    heading: { type: String },
    subtitle: { type: String },
  };

  static styles = css`
    :host {
      display: flex;
      align-items: center;
      gap: var(--space-md);
      margin-bottom: var(--space-md);
      flex-wrap: wrap;
    }
    .titles { display: flex; flex-direction: column; gap: var(--space-xs); min-width: 0; }
    h1 {
      margin: 0;
      font-size: var(--text-xl);
      font-weight: var(--font-bold);
      color: var(--heading);
      line-height: var(--leading-snug);
    }
    .subtitle {
      font-size: var(--text-sm);
      color: var(--muted);
    }
    .actions {
      margin-inline-start: auto;
      display: flex;
      align-items: center;
      gap: var(--space-sm);
      flex-wrap: wrap;
    }
  `;

  constructor() {
    super();
    this.heading = '';
    this.subtitle = '';
  }

  render() {
    return html`
      <div class="titles">
        <h1>${this.heading}</h1>
        ${this.subtitle ? html`<span class="subtitle">${this.subtitle}</span>` : ''}
      </div>
      <div class="actions"><slot></slot></div>
    `;
  }
}

customElements.define('dc-page-header', DcPageHeader);
