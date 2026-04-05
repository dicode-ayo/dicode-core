import type { DicodeSdk } from "../../sdk.ts";
import {
  buildAuthUrl, exchangeCodePKCE, exchangeCodePKCEWithSecret,
  generatePKCE, handleAuthNeeded, successHtml, tokenExpiresWithin,
} from "../_oauth/flow.ts";
import { Looker } from "../_oauth/providers.ts";

// Looker OAuth2 — PKCE (client_secret optional; some Looker setups require it).
// Auth and token URLs are instance-specific — set the `looker_instance` param.
// Looker access tokens expire in 1 hour; no refresh tokens are issued.
// Configure an OAuth app in Looker Admin → Users → OAuth Applications.

const EXPIRY_KV_KEY = "looker_oauth_expires_at";

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const instance      = (await params.get("looker_instance") ?? Deno.env.get("LOOKER_INSTANCE") ?? "").replace(/^https?:\/\//, "");
  const clientId      = Deno.env.get("LOOKER_CLIENT_ID") ?? "";
  const clientSecret  = Deno.env.get("LOOKER_CLIENT_SECRET") ?? "";
  const baseURL       = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const existingToken = Deno.env.get("LOOKER_ACCESS_TOKEN");

  if (!instance) throw new Error("Set the `looker_instance` param or LOOKER_INSTANCE secret (e.g. company.cloud.looker.com)");
  if (!clientId)  throw new Error("Set LOOKER_CLIENT_ID secret (from Looker Admin → Users → OAuth Applications)");

  const provider    = Looker(instance);
  const redirectURI = `${baseURL}/hooks/looker-oauth`;
  const scope       = await params.get("scope") ?? "";
  const inp         = input as Record<string, unknown> | null;
  const code        = inp?.code as string | undefined;

  if (code) {
    const verifier = await kv.get("looker_oauth_verifier") as string | null;
    if (!verifier) return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    await kv.delete("looker_oauth_verifier");
    const tokens = clientSecret
      ? await exchangeCodePKCEWithSecret({ provider, clientId, clientSecret, redirectURI, code, verifier })
      : await exchangeCodePKCE({ provider, clientId, redirectURI, code, verifier });
    await dicode.secrets_set("LOOKER_ACCESS_TOKEN", tokens.access_token);
    if (tokens.expires_in) await kv.set(EXPIRY_KV_KEY, Date.now() + tokens.expires_in * 1000);
    return output.html(successHtml("Looker", ["LOOKER_ACCESS_TOKEN"]));
  }

  // Looker doesn't issue refresh tokens — re-auth when expired.
  if (existingToken) {
    const expiresAt = (await kv.get(EXPIRY_KV_KEY)) as number | null;
    if (!tokenExpiresWithin(expiresAt, 60)) {
      if (inp !== null) return { valid: true };
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">Looker token still valid</h2>
        <p>Expires at ${new Date(expiresAt!).toISOString()}.</p></div>`);
    }
  }

  const { verifier, challenge } = await generatePKCE();
  await kv.set("looker_oauth_verifier", verifier);
  const authURL = buildAuthUrl({ provider, clientId, redirectURI, scope,
    extra: { code_challenge: challenge, code_challenge_method: "S256" } });

  return handleAuthNeeded({
    name: "Looker", authURL, redirectURI, scope, color: "#4285F4",
    isChain: inp !== null, output, dicode,
  });
}
