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
      gap: var(--space-md, 1rem);
      margin-bottom: var(--space-md, 1rem);
      flex-wrap: wrap;
    }
    .titles { display: flex; flex-direction: column; gap: .15rem; min-width: 0; }
    h1 {
      margin: 0;
      font-size: var(--text-xl, 1.4rem);
      font-weight: var(--font-bold, 700);
      color: var(--heading, #fff);
      line-height: var(--leading-snug, 1.3);
    }
    .subtitle {
      font-size: var(--text-sm, .82rem);
      color: var(--muted, #8b93a8);
    }
    .actions {
      margin-left: auto;
      display: flex;
      align-items: center;
      gap: var(--space-sm, .5rem);
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
