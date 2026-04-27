import type { DicodeSdk } from "../../sdk.ts";

interface ProviderMeta {
  key:        string;
  label:      string;
  color:      string;
  // standalone === true means the provider is NOT relay-broker-backed.
  // The Connect button opens the webhook URL directly; the per-provider
  // task renders an "Authorize with X" page. Currently only OpenRouter.
  standalone?: { webhookPath: string };
}

// KNOWN must stay in sync with the providers in tasks/auth/taskset.yaml.
// Adding a provider there means appending one row here (key/label/color),
// and vice versa. The colors here mirror the per-provider `color` params
// in that taskset; they are duplicated for now because the SPA renders the
// list before the per-provider task runs, so the dashboard cannot read
// them from the taskset directly. If the duplication becomes painful,
// extract a shared providers.json.
const KNOWN: ProviderMeta[] = [
  { key: "github",     label: "GitHub",     color: "#24292e" },
  { key: "google",     label: "Google",     color: "#4285f4" },
  { key: "slack",      label: "Slack",      color: "#4a154b" },
  { key: "spotify",    label: "Spotify",    color: "#1db954" },
  { key: "linear",     label: "Linear",     color: "#5e6ad2" },
  { key: "discord",    label: "Discord",    color: "#5865f2" },
  { key: "gitlab",     label: "GitLab",     color: "#fc6d26" },
  { key: "airtable",   label: "Airtable",   color: "#fcb400" },
  { key: "notion",     label: "Notion",     color: "#000000" },
  { key: "confluence", label: "Confluence", color: "#0052cc" },
  { key: "salesforce", label: "Salesforce", color: "#00a1e0" },
  { key: "stripe",     label: "Stripe",     color: "#635bff" },
  { key: "office365",  label: "Office365",  color: "#d83b01" },
  { key: "azure",      label: "Azure",      color: "#0078d4" },
  { key: "looker",     label: "Looker",     color: "#4285f4" },
  { key: "openrouter", label: "OpenRouter", color: "#6467f2",
    standalone: { webhookPath: "/hooks/openrouter-oauth" } },
];

const MAX_PROVIDERS = 64;

export default async function main({ params, input, dicode }: DicodeSdk) {
  const requested = ((await params.get("providers")) ?? "")
    .split(",").map(s => s.trim()).filter(Boolean);
  if (requested.length > MAX_PROVIDERS) {
    throw new Error(`at most ${MAX_PROVIDERS} providers`);
  }

  const inp = (input ?? null) as Record<string, unknown> | null;
  const action = (inp?.action ?? "list") as string;

  if (action === "list") {
    if (requested.length === 0) return [];
    const statuses = await dicode.oauth.list_status(requested);
    const meta = new Map(KNOWN.map(m => [m.key, m]));
    return statuses.map(s => ({ ...s, meta: meta.get(s.provider) ?? null }));
  }

  if (action === "connect") {
    const p = String(inp?.provider ?? "");
    const m = KNOWN.find(k => k.key === p);
    if (!m) throw new Error(`unknown provider: ${p}`);

    if (m.standalone) {
      const baseURL = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
      return { provider: p, url: `${baseURL}${m.standalone.webhookPath}` };
    }

    const run = await dicode.run_task("buildin/auth-start", { provider: p });
    const ret = (run as { returnValue?: { url?: string; session_id?: string } })?.returnValue;
    if (!ret?.url) throw new Error(`buildin/auth-start did not return a url for ${p}`);
    return { provider: p, url: ret.url, session_id: ret.session_id };
  }

  throw new Error(`unknown action: ${action}`);
}
