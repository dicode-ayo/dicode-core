import type { DicodeSdk, KV } from "../../sdk.ts";

// Reconcile the Cloudflare edge for a bootstrap relay deployment. Two records,
// two roles, one hard rule: the mTLS control channel must reach the host with
// its TLS client cert intact, so its DNS record must never be proxied — a
// Cloudflare proxy/tunnel terminates TLS and strips the cert, and the broker
// then rejects every daemon with close 4401.

const CF_BASE = "https://api.cloudflare.com/client/v4";

type FetchFn = typeof globalThis.fetch;

// deno-lint-ignore no-explicit-any
type CF = (method: string, path: string, body?: unknown) => Promise<any>;

export interface DesiredRecord {
  role: "control" | "public";
  type: "A" | "CNAME";
  name: string;
  content: string;
  proxied: boolean;
  ttl: number;
}

export interface PlanEntry {
  action: "create" | "update" | "tunnel-create";
  name: string;
  from: unknown;
  to: unknown;
}

/** CF API client over the CF `{success, result, errors}` envelope. */
function makeCF(token: string, fetchFn: FetchFn = globalThis.fetch): CF {
  return async (method, path, body) => {
    const res = await fetchFn(`${CF_BASE}${path}`, {
      method,
      headers: {
        "Authorization": `Bearer ${token}`,
        "Content-Type": "application/json",
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const env = await res.json();
    if (!env || env.success !== true) {
      const detail = env?.errors ? JSON.stringify(env.errors) : `HTTP ${res.status}`;
      throw new Error(`Cloudflare API ${method} ${path} failed: ${detail}`);
    }
    return env.result;
  };
}

async function resolveZoneId(cf: CF, zone: string): Promise<string> {
  const zones = await cf("GET", `/zones?name=${encodeURIComponent(zone)}`);
  if (!Array.isArray(zones) || zones.length === 0) {
    throw new Error(`zone not found: ${zone}`);
  }
  if (zones.length > 1) {
    throw new Error(`ambiguous zone: ${zone} matched ${zones.length} zones`);
  }
  return zones[0].id as string;
}

// Desired state is derived from each record's role — `proxied` is never a free
// input, so no caller (or param drift) can flip the control channel to proxied.
export function buildDesiredRecords(opts: {
  control_hostname: string;
  control_ip: string;
  public_hostname: string;
  tunnelId: string | null;
}): DesiredRecord[] {
  const desired: DesiredRecord[] = [
    { role: "control", type: "A", name: opts.control_hostname, content: opts.control_ip, proxied: false, ttl: 1 },
  ];
  if (opts.tunnelId) {
    desired.push({
      role: "public",
      type: "CNAME",
      name: opts.public_hostname,
      content: `${opts.tunnelId}.cfargotunnel.com`,
      proxied: true,
      ttl: 1,
    });
  }
  return desired;
}

// Guard the invariant before any mutation: control ⇒ never proxied, public ⇒
// always proxied. Kept separate and exported so it is unit-tested in isolation.
export function assertInvariant(desired: DesiredRecord[]): void {
  for (const r of desired) {
    if (r.role === "control" && r.proxied !== false) {
      throw new Error(
        `INVARIANT: control-channel record ${r.name} must never be proxied (Cloudflare would strip the mTLS client cert → close 4401)`,
      );
    }
    if (r.role === "public" && r.proxied !== true) {
      throw new Error(
        `INVARIANT: public record ${r.name} must be proxied to route through the Cloudflare Tunnel`,
      );
    }
  }
}

async function ensureTunnel(cf: CF, account: string, opts: {
  tunnel_name: string;
  public_hostname: string;
  local_port: number;
  ingress_host: string;
  dry_run: boolean;
  plan: PlanEntry[];
  kv: KV;
}): Promise<{ tunnelId: string | null; tokenStored: boolean }> {
  const { tunnel_name, public_hostname, local_port, ingress_host, dry_run, plan, kv } = opts;
  const service = `http://${ingress_host}:${local_port}`;
  const existing = await cf(
    "GET",
    `/accounts/${account}/cfd_tunnel?name=${encodeURIComponent(tunnel_name)}&is_deleted=false`,
  );
  if (Array.isArray(existing) && existing.length > 0) {
    return { tunnelId: existing[0].id as string, tokenStored: false };
  }

  if (dry_run) {
    plan.push({
      action: "tunnel-create",
      name: tunnel_name,
      from: null,
      to: `create tunnel + ingress ${public_hostname} → ${service}`,
    });
    return { tunnelId: null, tokenStored: false };
  }

  const created = await cf("POST", `/accounts/${account}/cfd_tunnel`, {
    name: tunnel_name,
    config_src: "cloudflare",
  });
  const tunnelId = created.id as string;
  // CF returns the connector token only at creation; persist it so the operator
  // (or a follow-up sidecar task) can run cloudflared without re-minting.
  await kv.set("tunnel_token", created.token);
  await cf("PUT", `/accounts/${account}/cfd_tunnel/${tunnelId}/configurations`, {
    config: {
      ingress: [
        { hostname: public_hostname, service },
        { service: "http_status:404" },
      ],
    },
  });
  plan.push({ action: "tunnel-create", name: tunnel_name, from: null, to: tunnelId });
  return { tunnelId, tokenStored: true };
}

// Reconcile only the records we own — never delete an unmanaged record.
async function reconcileDns(
  cf: CF,
  zid: string,
  desired: DesiredRecord[],
  dry_run: boolean,
  plan: PlanEntry[],
): Promise<void> {
  for (const r of desired) {
    const found = await cf(
      "GET",
      `/zones/${zid}/dns_records?type=${r.type}&name=${encodeURIComponent(r.name)}`,
    );
    const body = { type: r.type, name: r.name, content: r.content, proxied: r.proxied, ttl: r.ttl };

    if (!Array.isArray(found) || found.length === 0) {
      plan.push({ action: "create", name: r.name, from: null, to: { content: r.content, proxied: r.proxied } });
      if (!dry_run) await cf("POST", `/zones/${zid}/dns_records`, body);
      continue;
    }

    const cur = found[0];
    if (cur.content === r.content && cur.proxied === r.proxied) continue;

    plan.push({
      action: "update",
      name: r.name,
      from: { content: cur.content, proxied: cur.proxied },
      to: { content: r.content, proxied: r.proxied },
    });
    if (!dry_run) await cf("PATCH", `/zones/${zid}/dns_records/${cur.id}`, body);
  }
}

export default async function main({ params, kv }: DicodeSdk) {
  const token = Deno.env.get("CLOUDFLARE_API_TOKEN");
  const account = Deno.env.get("CLOUDFLARE_ACCOUNT_ID");
  if (!token) throw new Error("CLOUDFLARE_API_TOKEN is not set");
  if (!account) throw new Error("CLOUDFLARE_ACCOUNT_ID is not set");

  const zone = (await params.get("zone")) ?? "";
  const public_hostname = (await params.get("public_hostname")) ?? "";
  const control_hostname = (await params.get("control_hostname")) ?? "";
  const control_ip = (await params.get("control_ip")) ?? "";
  const tunnel_name = (await params.get("tunnel_name")) ?? "";
  const local_port = Number((await params.get("local_port")) ?? "5553");
  const ingress_host = (await params.get("ingress_host")) ?? "localhost";
  // Default (and the cron drift run) is safe: dry_run unless explicitly false.
  const dry_run = (await params.get("dry_run")) !== "false";

  const missing = Object.entries({ zone, public_hostname, control_hostname, control_ip })
    .filter(([, v]) => !v)
    .map(([k]) => k);
  if (missing.length) throw new Error(`missing required params: ${missing.join(", ")}`);

  const cf = makeCF(token);
  const plan: PlanEntry[] = [];

  const zid = await resolveZoneId(cf, zone);

  let tunnelId: string | null = null;
  let tokenStored = false;
  if (tunnel_name) {
    const t = await ensureTunnel(cf, account, { tunnel_name, public_hostname, local_port, ingress_host, dry_run, plan, kv });
    tunnelId = t.tunnelId;
    tokenStored = t.tokenStored;
  }

  const desired = buildDesiredRecords({ control_hostname, control_ip, public_hostname, tunnelId });
  assertInvariant(desired);

  await reconcileDns(cf, zid, desired, dry_run, plan);

  const summary = plan.length === 0
    ? `relay-edge: no drift in zone ${zone}`
    : `relay-edge: ${plan.length} change(s) in zone ${zone} — ${plan.map((p) => `${p.action} ${p.name}`).join(", ")}`;

  if (dry_run) {
    console.log(summary);
    for (const p of plan) console.log(`plan: ${p.action} ${p.name}`);
    return { dry_run: true, drift: plan.length > 0, changes: plan, summary };
  }

  console.log(summary);
  for (const p of plan) console.log(`applied: ${p.action} ${p.name}`);
  return { dry_run: false, applied: plan, tunnel_token_stored: tokenStored, summary };
}
