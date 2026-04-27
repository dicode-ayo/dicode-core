// api.js — thin wrappers around the auth-providers webhook endpoint.
//   - list()                       → GET  ?action=list  → ProviderRow[]
//   - connect(provider)            → POST { action: "connect", provider }
//                                    → { provider, url, session_id? }
// Errors throw with the daemon's error string.

const ENDPOINT = window.location.pathname.replace(/\/$/, "") || "/hooks/auth-providers";

async function fetchJson(method, body) {
  const url = method === "GET"
    ? `${ENDPOINT}?action=${encodeURIComponent(body.action)}`
    : ENDPOINT;
  const init = {
    method,
    headers: { "Accept": "application/json" },
  };
  if (method === "POST") {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const res = await fetch(url, init);
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(`HTTP ${res.status}: ${text || res.statusText}`);
  }
  // The trigger engine wraps task return values in { result, ... }; the
  // task return is the value we want.
  const envelope = await res.json();
  return envelope?.result ?? envelope;
}

export const api = {
  list:    ()                => fetchJson("GET",  { action: "list" }),
  connect: (provider)        => fetchJson("POST", { action: "connect", provider }),
};
