import { LitElement, html } from 'https://esm.sh/lit@3';
import { get } from '../lib/api.js';

// dc-nav renders additional <a> nav entries contributed by tasks via their
// task.yaml `webui.nav` block (#222). It fetches /api/tasks, picks kind:
// Task entries with a webui.nav.label set and a webhook trigger, and renders
// one plain root-relative <a href="/hooks/..."> per entry — the app.js click
// interceptor leaves root-relative hrefs alone, so these are ordinary full
// page navigations into the contributing task's own webhook-served SPA.
class DcNav extends LitElement {
  // Render into light DOM (not a shadow root) so the header's global
  // `nav a` styles in global.css apply to these links exactly like the
  // static ones — matches dc-theme-toggle.js / dc-task-list.js.
  createRenderRoot() { return this; }

  static properties = {
    _entries: { state: true },
  };

  constructor() {
    super();
    this._entries = [];
  }

  connectedCallback() {
    super.connectedCallback();
    this._load();
  }

  async _load() {
    let tasks;
    try {
      tasks = await get('/api/tasks');
    } catch (e) {
      // Don't break the rest of the header on a transient /api/tasks
      // failure (e.g. a 401 before the auth overlay resolves) — the
      // static nav links still work.
      console.warn('[dc-nav] failed to load /api/tasks:', e.message || e);
      return;
    }

    const entries = [];
    for (const t of tasks || []) {
      if (t.kind !== 'Task') continue;
      const label = t.webui?.nav?.label;
      if (!label) continue;
      const path = t.trigger?.Webhook;
      if (!path) {
        console.warn(`[dc-nav] task ${t.id} declares webui.nav but has no trigger.webhook; skipping`);
        continue;
      }
      entries.push({
        id: t.id,
        label,
        order: t.webui.nav.order || 0,
        path,
      });
    }

    entries.sort((a, b) => (a.order - b.order) || a.id.localeCompare(b.id));
    this._entries = entries;
  }

  render() {
    return html`${this._entries.map(e => html`<a href=${e.path}>${e.label}</a>`)}`;
  }
}

customElements.define('dc-nav', DcNav);
