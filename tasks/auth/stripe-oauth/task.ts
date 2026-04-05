import type { DicodeSdk } from "../../sdk.ts";
import {
  exchangeCodeSecret, generatePKCE, handleAuthNeeded, successHtml,
  tokenExpiresWithin,
} from "../_oauth/flow.ts";
import { Stripe as provider } from "../_oauth/providers.ts";

// Stripe Connect OAuth — secret only (no PKCE support).
// This task connects a Stripe account to your platform via Stripe Connect.
// client_id  = your platform's Connect application client ID ("ca_xxx")
// client_secret = your platform's secret key ("sk_live_xxx" or "sk_test_xxx")
// Stores: STRIPE_ACCESS_TOKEN (1h), STRIPE_REFRESH_TOKEN, STRIPE_ACCOUNT_ID
// Refresh tokens are rotated on each use and expire after 1 year of non-use.

const EXPIRY_KV_KEY = "stripe_oauth_expires_at";

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const clientId        = Deno.env.get("STRIPE_CLIENT_ID")!;
  const clientSecret    = Deno.env.get("STRIPE_SECRET_KEY")!;
  const baseURL         = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const existingToken   = Deno.env.get("STRIPE_ACCESS_TOKEN");
  const existingRefresh = Deno.env.get("STRIPE_REFRESH_TOKEN");
  const redirectURI     = `${baseURL}/hooks/stripe-oauth`;
  const scope           = await params.get("scope") ?? "read_write";
  const inp             = input as Record<string, unknown> | null;
  const code            = inp?.code as string | undefined;

  if (code) {
    const tokens = await exchangeCodeSecret({ provider, clientId, clientSecret, redirectURI, code });
    const resp = tokens as Record<string, unknown>;
    await dicode.secrets_set("STRIPE_ACCESS_TOKEN", tokens.access_token);
    if (tokens.refresh_token) await dicode.secrets_set("STRIPE_REFRESH_TOKEN", tokens.refresh_token);
    if (typeof resp.stripe_user_id === "string") await dicode.secrets_set("STRIPE_ACCOUNT_ID", resp.stripe_user_id);
    if (tokens.expires_in) await kv.set(EXPIRY_KV_KEY, Date.now() + tokens.expires_in * 1000);
    const stored = ["STRIPE_ACCESS_TOKEN"];
    if (tokens.refresh_token) stored.push("STRIPE_REFRESH_TOKEN");
    if (resp.stripe_user_id) stored.push("STRIPE_ACCOUNT_ID");
    return output.html(successHtml("Stripe Connect", stored));
  }

  if (existingToken) {
    const expiresAt = (await kv.get(EXPIRY_KV_KEY)) as number | null;
    if (!tokenExpiresWithin(expiresAt, 60)) {
      if (inp !== null) return { valid: true };
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">Stripe token still valid</h2>
        <p>Expires at ${new Date(expiresAt!).toISOString()}.</p></div>`);
    }
    // Refresh using the standard refresh_token grant
    if (existingRefresh) {
      try {
        const res = await fetch(provider.tokenUrl, {
          method: "POST",
          headers: { "Content-Type": "application/x-www-form-urlencoded" },
          body: new URLSearchParams({
            grant_type:    "refresh_token",
            client_secret: clientSecret,
            refresh_token: existingRefresh,
          }),
        });
        const refreshed = await res.json() as Record<string, unknown>;
        if (!res.ok) throw new Error(`${refreshed.error}: ${refreshed.error_description}`);
        await dicode.secrets_set("STRIPE_ACCESS_TOKEN", refreshed.access_token as string);
        if (refreshed.refresh_token) await dicode.secrets_set("STRIPE_REFRESH_TOKEN", refreshed.refresh_token as string);
        if (refreshed.expires_in) await kv.set(EXPIRY_KV_KEY, Date.now() + (refreshed.expires_in as number) * 1000);
        console.log("Stripe access token refreshed silently");
        if (inp !== null) return { refreshed: true };
        return output.html(successHtml("Stripe Connect (refreshed)", ["STRIPE_ACCESS_TOKEN"]));
      } catch (err) {
        console.error("Silent refresh failed:", err);
      }
    }
  }

  // Stripe Connect auth URL (no PKCE)
  const state  = (await generatePKCE()).verifier; // random state value for CSRF protection
  await kv.set("stripe_oauth_state", state);
  const authURL = `${provider.authUrl}?${new URLSearchParams({
    response_type: "code",
    client_id:     clientId,
    scope,
    redirect_uri:  redirectURI,
    state,
  })}`;

  return handleAuthNeeded({
    name: "Stripe", authURL, redirectURI, scope, color: "#635BFF",
    isChain: inp !== null, output, dicode,
  });
}
