// api.js — thin wrappers around the auth-providers webhook endpoint.
//   - list()                       → POST { action: "list" }       → ProviderRow[]
//   - connect(provider)            → POST { action: "connect", provider }
//                                    → { provider, url, session_id? }
// Errors throw with the daemon's error string.
//
// Both calls use POST because the trigger engine's GET handler unconditionally
// serves index.html when one is present (pkg/trigger/engine.go) — there is no
// Accept: application/json bypass. Using POST is also more REST-correct for
// "do something" requests (list-with-side-effect-free is debatable, but the
// uniform method keeps the wrapper trivial).

const ENDPOINT = window.location.pathname.replace(/\/$/, "") || "/hooks/auth-providers";

async function postJson(body) {
  const res = await fetch(ENDPOINT, {
    method: "POST",
    headers: {
      "Accept":       "application/json",
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
  });
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
  list:    ()         => postJson({ action: "list" }),
  connect: (provider) => postJson({ action: "connect", provider }),
};
