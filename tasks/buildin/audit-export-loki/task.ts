// Ships the security audit trail to Grafana Loki (#415).
//
// Poll-batch-push: read the last cursor from kv, pull the next batch of
// audit events in ascending order, push to Loki's /loki/api/v1/push, and
// advance the cursor ONLY on a 2xx. At-least-once delivery — dedupe
// downstream on the `id` field carried on every line.

import type { AuditEvent, DicodeSdk } from "../../sdk.ts";

// Persisted resume position. The opaque cursor is whatever dicode.audit
// returns as next_cursor; we treat it as a black box.
const CURSOR_KEY = "cursor";

// Loki ingests nanosecond unix timestamps as decimal strings.
function toLokiTs(iso: string): string {
  const ms = Date.parse(iso);
  const base = Number.isFinite(ms) ? ms : Date.now();
  return String(base) + "000000";
}

// Group events into Loki streams keyed by the low-cardinality labels.
// High-cardinality data (id, run_id, actor/target ids) stays in the log
// line, not in labels, to avoid Loki stream explosion.
function buildStreams(events: AuditEvent[]) {
  const byLabel = new Map<string, { stream: Record<string, string>; values: [string, string][] }>();
  for (const ev of events) {
    const labels: Record<string, string> = {
      job: "dicode-audit",
      event_type: ev.event_type || "unknown",
      actor_kind: ev.actor_kind || "unknown",
      target_kind: ev.target_kind || "unknown",
      allowed: ev.allowed ? "true" : "false",
    };
    const key = JSON.stringify(labels);
    let bucket = byLabel.get(key);
    if (!bucket) {
      bucket = { stream: labels, values: [] };
      byLabel.set(key, bucket);
    }
    bucket.values.push([toLokiTs(ev.ts), JSON.stringify(ev)]);
  }
  return Array.from(byLabel.values());
}

export default async function main({ params, kv, dicode }: DicodeSdk) {
  const endpoint = ((await params.get("endpoint")) ?? "").trim().replace(/\/+$/, "");
  if (!endpoint) {
    return { ok: false, error: "endpoint param is required" };
  }

  const limitRaw = (await params.get("batch_limit")) ?? "1000";
  let limit = Number(limitRaw);
  if (!Number.isFinite(limit) || limit <= 0) limit = 1000;
  if (limit > 1000) limit = 1000;

  const cursor = ((await kv.get(CURSOR_KEY)) as string | undefined) ?? "";

  const res = await dicode.audit.query({ after: cursor, order: "asc", limit });
  const events = res?.events ?? [];
  if (events.length === 0) {
    return { ok: true, shipped: 0, cursor };
  }

  const body = JSON.stringify({ streams: buildStreams(events) });

  const headers: Record<string, string> = { "content-type": "application/json" };
  const tenant = ((await params.get("tenant_id")) ?? "").trim();
  if (tenant) headers["X-Scope-OrgID"] = tenant;

  // Auth: basic when auth_user is set (token is the password), else bearer.
  // The token is never a param — it is resolved from the secrets store /
  // host env into LOKI_AUTH_TOKEN by the daemon.
  const token = Deno.env.get("LOKI_AUTH_TOKEN") ?? "";
  const authUser = ((await params.get("auth_user")) ?? "").trim();
  if (token) {
    if (authUser) {
      headers["authorization"] = "Basic " + btoa(`${authUser}:${token}`);
    } else {
      headers["authorization"] = "Bearer " + token;
    }
  }

  const resp = await fetch(`${endpoint}/loki/api/v1/push`, {
    method: "POST",
    headers,
    body,
  });

  if (resp.status < 200 || resp.status >= 300) {
    const text = await resp.text().catch(() => "");
    // Cursor is NOT advanced: the same batch is retried next tick.
    return {
      ok: false,
      shipped: 0,
      status: resp.status,
      error: `loki push failed: ${resp.status} ${text.slice(0, 200)}`,
    };
  }

  // 2xx — advance the cursor so the next run resumes after this batch.
  const next = res.next_cursor || cursor;
  await kv.set(CURSOR_KEY, next);

  console.log(`[audit-export-loki] shipped ${events.length} events; cursor -> ${next}`);
  return { ok: true, shipped: events.length, cursor: next };
}
