import type { DicodeSdk } from "../../sdk.ts";
import {
  buildAuthUrl, exchangeCodePKCEWithSecret, generatePKCE, handleAuthNeeded,
  refreshAccessTokenWithSecret, successHtml, tokenExpiresWithin,
} from "../_oauth/flow.ts";
import { Office365 as provider } from "../_oauth/providers.ts";

// Office 365 / Microsoft Graph OAuth2 — PKCE + client secret.
// The token covers all Microsoft Graph APIs: Mail, Calendar, Teams, OneDrive, SharePoint, etc.
// Microsoft requires client_secret for Desktop app type even when PKCE is used.
// Refresh tokens expire after 90 days of inactivity.
// See task.yaml for Azure app registration instructions.

const EXPIRY_KV_KEY = "office365_oauth_expires_at";

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const clientId        = Deno.env.get("OFFICE365_CLIENT_ID")!;
  const clientSecret    = Deno.env.get("OFFICE365_CLIENT_SECRET")!;
  const baseURL         = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const existingToken   = Deno.env.get("OFFICE365_ACCESS_TOKEN");
  const existingRefresh = Deno.env.get("OFFICE365_REFRESH_TOKEN");
  const redirectURI     = `${baseURL}/hooks/office365-oauth`;
  const scope           = await params.get("scope") ?? "offline_access User.Read Mail.Read Calendars.Read";
  const inp             = input as Record<string, unknown> | null;
  const code            = inp?.code as string | undefined;

  if (code) {
    const verifier = await kv.get("office365_oauth_verifier") as string | null;
    if (!verifier) return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    await kv.delete("office365_oauth_verifier");
    const tokens = await exchangeCodePKCEWithSecret({
      provider, clientId, clientSecret, redirectURI, code, verifier,
    });
    await storeTokens(tokens, kv, dicode);
    const stored = ["OFFICE365_ACCESS_TOKEN"];
    if (tokens.refresh_token) stored.push("OFFICE365_REFRESH_TOKEN");
    return output.html(successHtml("Microsoft / Office 365", stored));
  }

  if (existingToken) {
    const expiresAt = (await kv.get(EXPIRY_KV_KEY)) as number | null;
    if (!tokenExpiresWithin(expiresAt, 60)) {
      if (inp !== null) return { valid: true };
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">Office 365 token still valid</h2>
        <p>Expires at ${new Date(expiresAt!).toISOString()}.</p></div>`);
    }
    if (existingRefresh) {
      try {
        const tokens = await refreshAccessTokenWithSecret({
          provider, clientId, clientSecret, refreshToken: existingRefresh,
        });
        await storeTokens(tokens, kv, dicode);
        console.log("Office 365 access token refreshed silently");
        if (inp !== null) return { refreshed: true };
        return output.html(successHtml("Microsoft (refreshed)", ["OFFICE365_ACCESS_TOKEN"]));
      } catch (err) {
        console.error("Silent refresh failed:", err);
      }
    }
  }

  const { verifier, challenge } = await generatePKCE();
  await kv.set("office365_oauth_verifier", verifier);
  const authURL = buildAuthUrl({
    provider, clientId, redirectURI, scope,
    extra: { code_challenge: challenge, code_challenge_method: "S256" },
  });

  return handleAuthNeeded({
    name: "Microsoft", authURL, redirectURI, scope, color: "#0078D4",
    isChain: inp !== null, output, dicode,
  });
}

async function storeTokens(
  tokens: { access_token: string; refresh_token?: string; expires_in?: number },
  kv: DicodeSdk["kv"],
  dicode: DicodeSdk["dicode"],
) {
  await dicode.secrets_set("OFFICE365_ACCESS_TOKEN", tokens.access_token);
  if (tokens.refresh_token) await dicode.secrets_set("OFFICE365_REFRESH_TOKEN", tokens.refresh_token);
  if (tokens.expires_in) await kv.set(EXPIRY_KV_KEY, Date.now() + tokens.expires_in * 1000);
}
