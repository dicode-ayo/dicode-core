import type { DicodeSdk } from "../../sdk.ts";
import {
  buildAuthUrl, exchangeCodePKCE, generatePKCE, handleAuthNeeded,
  refreshAccessTokenPKCE, successHtml, tokenExpiresWithin,
} from "../_oauth/flow.ts";
import { Discord as provider } from "../_oauth/providers.ts";
import { resolveClientId } from "../_oauth/builtin.ts";

const EXPIRY_KV_KEY = "discord_oauth_expires_at";

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const clientId        = resolveClientId("discord", "DISCORD_CLIENT_ID");
  const baseURL         = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const existingToken   = Deno.env.get("DISCORD_ACCESS_TOKEN");
  const existingRefresh = Deno.env.get("DISCORD_REFRESH_TOKEN");
  const redirectURI     = `${baseURL}/hooks/discord-oauth`;
  const scope           = await params.get("scope") ?? "identify email";
  const inp             = input as Record<string, unknown> | null;
  const code            = inp?.code as string | undefined;

  if (code) {
    const verifier = await kv.get("discord_oauth_verifier") as string | null;
    if (!verifier) return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    await kv.delete("discord_oauth_verifier");
    const tokens = await exchangeCodePKCE({ provider, clientId, redirectURI, code, verifier });
    await storeTokens(tokens, kv, dicode);
    const stored = ["DISCORD_ACCESS_TOKEN"];
    if (tokens.refresh_token) stored.push("DISCORD_REFRESH_TOKEN");
    return output.html(successHtml("Discord", stored));
  }

  if (existingToken) {
    const expiresAt = (await kv.get(EXPIRY_KV_KEY)) as number | null;
    if (!tokenExpiresWithin(expiresAt, 60)) {
      if (inp !== null) return { valid: true };
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">Discord token still valid</h2>
        <p>Expires at ${new Date(expiresAt!).toISOString()}.</p></div>`);
    }
    if (existingRefresh) {
      try {
        const tokens = await refreshAccessTokenPKCE({ provider, clientId, refreshToken: existingRefresh });
        await storeTokens(tokens, kv, dicode);
        console.log("Discord access token refreshed silently");
        if (inp !== null) return { refreshed: true };
        return output.html(successHtml("Discord (refreshed)", ["DISCORD_ACCESS_TOKEN"]));
      } catch (err) {
        console.error("Silent refresh failed:", err);
      }
    }
  }

  const { verifier, challenge } = await generatePKCE();
  await kv.set("discord_oauth_verifier", verifier);
  const authURL = buildAuthUrl({ provider, clientId, redirectURI, scope,
    extra: { code_challenge: challenge, code_challenge_method: "S256" } });

  return handleAuthNeeded({
    name: "Discord", authURL, redirectURI, scope, color: "#5865F2",
    isChain: inp !== null, output, dicode,
  });
}

async function storeTokens(
  tokens: { access_token: string; refresh_token?: string; expires_in?: number },
  kv: DicodeSdk["kv"],
  dicode: DicodeSdk["dicode"],
) {
  await dicode.secrets_set("DISCORD_ACCESS_TOKEN", tokens.access_token);
  if (tokens.refresh_token) await dicode.secrets_set("DISCORD_REFRESH_TOKEN", tokens.refresh_token);
  if (tokens.expires_in) await kv.set(EXPIRY_KV_KEY, Date.now() + tokens.expires_in * 1000);
}
