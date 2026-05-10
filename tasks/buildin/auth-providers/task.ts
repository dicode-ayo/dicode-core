import type { DicodeSdk } from "../../sdk.ts";

// BrokerProvider is the shape returned by the relay broker's GET /providers
// endpoint (dicode-relay >= 0.1.5).
interface BrokerProvider {
  key:             string;
  pkce:            boolean;
  scopes:          string[];
  secret_required: boolean;
  configured:      boolean;
}

// fetchBrokerProviders retrieves the provider catalogue from the relay broker.
// The endpoint is unauthenticated and returns no secret values.
async function fetchBrokerProviders(brokerURL: string): Promise<BrokerProvider[]> {
  const base = brokerURL.replace(/\/$/, "");
  const res = await fetch(`${base}/providers`);
  if (!res.ok) {
    throw new Error(`broker /providers returned ${res.status}: ${await res.text()}`);
  }
  return await res.json() as BrokerProvider[];
}

// checkConnected returns true when the provider's access token is present in
// the secrets store. Convention: auth-relay/task.ts writes
// <PROVIDER_UPPER>_ACCESS_TOKEN.  Closes #255.
//
// IPC errors are logged and degrade to has_token=false per-provider rather
// than failing the whole list — one misconfigured provider should not blank
// the dashboard for every other provider. The error is surfaced in run logs
// so operators can investigate (typical cause: missing
// permissions.dicode.secrets_has on a forked task).
async function checkConnected(dicode: DicodeSdk["dicode"], providerKey: string): Promise<boolean> {
  const secretName = providerKey.toUpperCase() + "_ACCESS_TOKEN";
  try {
    return await dicode.secrets.has(secretName);
  } catch (err) {
    console.error(
      `auth-providers: dicode.secrets.has(${secretName}) failed; reporting has_token=false. ` +
      `Cause: ${err instanceof Error ? err.message : String(err)}`,
    );
    return false;
  }
}

// STANDALONE maps provider keys that are NOT relay-broker-backed AND NOT
// instantiated from the _oauth-app template. Today this is just openrouter,
// which has its own bespoke task (no template marker). Anything inherited
// from _oauth-app is auto-discovered via list_tasks below — no need to
// hardcode it here.
const STANDALONE: Record<string, { webhookPath: string; label: string; color: string }> = {
  openrouter: {
    webhookPath: "/hooks/openrouter-oauth",
    label: "OpenRouter",
    color: "#6467f2",
  },
};

// OAUTH_APP_TEMPLATE is the marker set in tasks/auth/_oauth-app/task.yaml.
// Every taskset entry that inherits via `ref.path: ./_oauth-app/task.yaml`
// gets this propagated onto its merged spec by the resolver. The dashboard
// uses it to surface BYO OAuth tasks without hardcoding an allowlist —
// operators with their own taskset entry just appear automatically.
const OAUTH_APP_TEMPLATE = "dicode.io/oauth-app";

// paramDefault returns the default value of a named param from a list_tasks
// summary's params field. The IPC server passes through task.Params (a list
// of Param structs) verbatim; we only need the default value (set via
// taskset overrides per provider).
function paramDefault(params: unknown, name: string): string {
  if (!Array.isArray(params)) return "";
  for (const p of params) {
    if (p && typeof p === "object" && (p as { name?: string }).name === name) {
      return String((p as { default?: unknown }).default ?? "");
    }
  }
  return "";
}

// scanInheritors enumerates every registered task whose merged spec carries
// the `dicode.io/oauth-app` template marker, and shapes each as a
// dashboard-style provider entry. Returns the same canonical shape the
// STANDALONE branch emits so the panel renders them identically.
async function scanInheritors(
  dicode: DicodeSdk["dicode"],
  requested: Set<string>,
): Promise<Array<Record<string, unknown>>> {
  let tasks: Array<{
    id: string; name: string; template?: string; webhook?: string;
    enabled: boolean; params?: unknown;
  }>;
  try {
    tasks = await dicode.list_tasks();
  } catch (err) {
    console.error(
      `auth-providers: dicode.list_tasks failed; BYO OAuth tasks will not appear ` +
      `in the panel. Cause: ${err instanceof Error ? err.message : String(err)}`,
    );
    return [];
  }

  const out: Array<Record<string, unknown>> = [];
  for (const t of tasks) {
    if (t.template !== OAUTH_APP_TEMPLATE) continue;
    if (!t.enabled) continue;                     // skip disabled inheritors
    const key = paramDefault(t.params, "provider");
    if (!key) continue;                           // misconfigured BYO entry
    if (requested.size > 0 && !requested.has(key)) continue;
    if (!t.webhook) continue;                     // need a webhook to Connect
    out.push({
      key,
      pkce: true,
      scopes: [] as string[],
      secret_required: paramDefault(t.params, "client_secret_env") !== "",
      configured: true,
      has_token: await checkConnected(dicode, key),
      label: t.name,
      color: paramDefault(t.params, "color") || "#666",
      standalone: { webhookPath: t.webhook },
    });
  }
  return out;
}

export default async function main({ params, input, dicode }: DicodeSdk) {
  const inp = (input ?? null) as Record<string, unknown> | null;
  const action = (inp?.action ?? "list") as string;

  if (action === "list") {
    const requested = new Set(
      ((await params.get("providers")) ?? "")
        .split(",").map(s => s.trim()).filter(Boolean),
    );

    // Broker URL is optional. When relay is disabled in dicode.yaml the env
    // var is unset; fall back to the STANDALONE catalogue so users running
    // BYO-without-relay still get a useful answer instead of a 5xx.
    const brokerURL = Deno.env.get("DICODE_RELAY_BROKER_URL");
    let withStatus: Array<BrokerProvider & { has_token: boolean }> = [];
    if (brokerURL) {
      const brokerProviders = await fetchBrokerProviders(brokerURL);
      const filtered = requested.size === 0
        ? brokerProviders
        : brokerProviders.filter(p => requested.has(p.key));
      withStatus = await Promise.all(filtered.map(async (p) => ({
        ...p,
        has_token: await checkConnected(dicode, p.key),
      })));
    }

    // Append standalone providers if explicitly requested.
    const standaloneEntries = await Promise.all(
      Object.entries(STANDALONE)
        .filter(([key]) => requested.size === 0 || requested.has(key))
        .map(async ([key, meta]) => ({
          key,
          pkce: true,
          scopes: [] as string[],
          secret_required: false,
          configured: true, // standalone never requires broker config
          has_token: await checkConnected(dicode, key),
          label: meta.label,
          color: meta.color,
          standalone: { webhookPath: meta.webhookPath },
        })),
    );

    // Auto-discovered BYO OAuth tasks (anything inheriting the _oauth-app
    // template from a taskset entry — built-in office365/looker, plus any
    // user-supplied entry from their own taskset).
    const inheritorEntries = await scanInheritors(dicode, requested);

    // Dedup by key. Precedence: broker > standalone > inheritors. The broker
    // is centrally managed and harder for an operator to misconfigure, so a
    // user's BYO `github` entry (mistake or intentional) does not shadow the
    // broker's. Standalone wins over inheritors so an operator who explicitly
    // ships an `openrouter-oauth` task in their taskset cannot accidentally
    // double-list it.
    const seen = new Set<string>();
    const result: Array<Record<string, unknown>> = [];
    for (const e of [...withStatus, ...standaloneEntries, ...inheritorEntries]) {
      const k = (e as { key?: string }).key;
      if (!k || seen.has(k)) continue;
      seen.add(k);
      result.push(e as Record<string, unknown>);
    }
    return result;
  }

  if (action === "connect") {
    const p = String(inp?.provider ?? "");
    const baseURL = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");

    // Standalone provider (e.g. openrouter): return the webhook URL directly.
    const standalone = STANDALONE[p];
    if (standalone) {
      return { provider: p, url: `${baseURL}${standalone.webhookPath}` };
    }

    // Auto-discovered _oauth-app inheritor (built-in office365/looker, or
    // any user BYO entry): open the inherited webhook directly.
    try {
      const tasks = await dicode.list_tasks();
      for (const t of tasks) {
        if (t.template !== OAUTH_APP_TEMPLATE || !t.enabled || !t.webhook) continue;
        if (paramDefault(t.params, "provider") === p) {
          return { provider: p, url: `${baseURL}${t.webhook}` };
        }
      }
    } catch (err) {
      console.error(
        `auth-providers: dicode.list_tasks failed during connect; falling back to broker. ` +
        `Cause: ${err instanceof Error ? err.message : String(err)}`,
      );
    }

    // Relay-broker provider: delegate to buildin/auth-start. Pre-empt the
    // less-friendly "relay disabled" error the auth-start task surfaces
    // when the broker isn't reachable.
    if (!Deno.env.get("DICODE_RELAY_BROKER_URL")) {
      throw new Error(
        `provider '${p}' requires the relay broker. Enable relay in dicode.yaml ` +
        `or instantiate a BYO OAuth task in your own taskset (see auth/_oauth-app).`,
      );
    }
    const run = await dicode.run_task("buildin/auth-start", { provider: p });
    const ret = (run as { returnValue?: { url?: string; session_id?: string } })?.returnValue;
    if (!ret?.url) throw new Error(`buildin/auth-start did not return a url for ${p}`);
    return { provider: p, url: ret.url, session_id: ret.session_id };
  }

  throw new Error(`unknown action: ${action}`);
}
