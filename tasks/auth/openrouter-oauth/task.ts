// OpenRouter PKCE flow — non-standard OAuth2:
//   * no client_id, no app registration
//   * callback_url is a request param, not pre-registered
//   * token exchange returns a long-lived API key, not access/refresh tokens
//
// This is why it doesn't fit _oauth-app and lives as a standalone task.

import type { DicodeSdk } from "../../sdk.ts";
import { generatePKCE, generateState, handleAuthNeeded, successHtml } from "../_oauth/flow.ts";

const AUTH_URL         = "https://openrouter.ai/auth";
const TOKEN_URL        = "https://openrouter.ai/api/v1/auth/keys";
const REDIRECT_SUFFIX  = "openrouter-oauth";
const SECRET_KEY       = "OPENROUTER_API_KEY";
const VERIFIER_KV_KEY  = "openrouter_oauth_verifier";
const STATE_KV_KEY     = "openrouter_oauth_state";

export default async function main({ input, output, kv, dicode }: DicodeSdk) {
  const baseURL     = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const redirectURI = `${baseURL}/hooks/${REDIRECT_SUFFIX}`;

  const inp  = (input ?? null) as Record<string, unknown> | null;
  const code = inp?.code as string | undefined;

  // ── Webhook leg: exchange code → store API key ──────────────────────────
  if (code) {
    const verifier      = await kv.get(VERIFIER_KV_KEY) as string | null;
    const expectedState = await kv.get(STATE_KV_KEY)    as string | null;

    if (!verifier) {
      return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    }
    if (expectedState && inp?.state && expectedState !== inp.state) {
      return output.html(`<p style="color:red">State mismatch — possible CSRF. Restart.</p>`);
    }

    await kv.delete(VERIFIER_KV_KEY);
    await kv.delete(STATE_KV_KEY);

    const res = await fetch(TOKEN_URL, {
      method:  "POST",
      headers: { "Content-Type": "application/json" },
      body:    JSON.stringify({ code, code_verifier: verifier, code_challenge_method: "S256" }),
    });
    if (!res.ok) {
      const detail = await res.text();
      throw new Error(`OpenRouter token exchange failed: ${res.status} ${detail}`);
    }
    const body = await res.json() as { key?: string; user_id?: string };
    if (!body.key) throw new Error(`OpenRouter response missing "key": ${JSON.stringify(body)}`);

    await dicode.secrets_set(SECRET_KEY, body.key);
    const stored = [SECRET_KEY];
    if (body.user_id) {
      await dicode.secrets_set("OPENROUTER_USER_ID", body.user_id);
      stored.push("OPENROUTER_USER_ID");
    }
    console.log(`[OpenRouter] stored ${stored.join(", ")}`);
    return output.html(successHtml("OpenRouter", stored));
  }

  // ── Already authorised? OpenRouter keys are long-lived ───────────────────
  const existing = Deno.env.get(SECRET_KEY);
  if (existing) {
    console.log("[OpenRouter] existing key found — re-run only to rotate");
    if (inp !== null) return { valid: true };
    return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
      <h2 style="color:#1a7f37">OpenRouter key already stored</h2>
      <p>Keys do not expire automatically. Re-run only to rotate or revoke.</p>
    </div>`);
  }

  // ── Need authorisation: generate PKCE + redirect user ────────────────────
  const state = generateState();
  const pkce  = await generatePKCE();
  await kv.set(STATE_KV_KEY, state);
  await kv.set(VERIFIER_KV_KEY, pkce.verifier);

  const authURL = new URL(AUTH_URL);
  authURL.searchParams.set("callback_url",          redirectURI);
  authURL.searchParams.set("code_challenge",        pkce.challenge);
  authURL.searchParams.set("code_challenge_method", "S256");
  authURL.searchParams.set("state",                 state);

  return handleAuthNeeded({
    name:        "OpenRouter",
    authURL:     authURL.toString(),
    redirectURI,
    scope:       "",
    color:       "#6467f2",
    isChain:     inp !== null,
    output,
    dicode,
  });
}
