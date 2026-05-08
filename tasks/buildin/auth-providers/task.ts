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
async function checkConnected(dicode: DicodeSdk["dicode"], providerKey: string): Promise<boolean> {
  const secretName = providerKey.toUpperCase() + "_ACCESS_TOKEN";
  try {
    return await dicode.secrets.has(secretName);
  } catch {
    // Fallback to false if the IPC call fails (e.g. during tests or when
    // secrets_has permission is not granted).
    return false;
  }
}

// STANDALONE maps provider keys that are NOT relay-broker-backed to their
// webhook path. These providers use PKCE directly without the broker, so they
// do not appear in the broker's /providers response but must still be listable
// when the task is invoked with their key in the `providers` param.
const STANDALONE: Record<string, { webhookPath: string; label: string; color: string }> = {
  openrouter: {
    webhookPath: "/hooks/openrouter-oauth",
    label: "OpenRouter",
    color: "#6467f2",
  },
};

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

    return [...withStatus, ...standaloneEntries];
  }

  if (action === "connect") {
    const p = String(inp?.provider ?? "");

    // Standalone provider: return the webhook URL directly.
    const standalone = STANDALONE[p];
    if (standalone) {
      const baseURL = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
      return { provider: p, url: `${baseURL}${standalone.webhookPath}` };
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
