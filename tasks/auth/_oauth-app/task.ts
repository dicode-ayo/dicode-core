// Generic OAuth 2.0 task — driven entirely by params set via taskset overrides.
// Supports PKCE-only, PKCE+secret, and secret-only flows, plus silent refresh
// and chain-aware notification.
//
// Instantiate via taskset.yaml entries:
//
//   google-oauth:
//     ref: { path: ./_oauth-app/task.yaml }
//     overrides:
//       name: Google OAuth
//       trigger: { webhook: /hooks/google-oauth }
//       params:
//         - { name: provider,          default: google }
//         - { name: scope,             default: "https://www.googleapis.com/auth/gmail.readonly" }
//         - { name: token_lifetime,    default: expires }
//         - { name: color,             default: "#4285f4" }
//         - { name: client_id_env,     default: CLIENT_ID }
//         - { name: client_secret_env, default: CLIENT_SECRET }
//         - { name: access_token_env,  default: GOOGLE_ACCESS_TOKEN }
//         - { name: refresh_token_env, default: GOOGLE_REFRESH_TOKEN }
//       env:
//         - { name: CLIENT_ID,            secret: GOOGLE_CLIENT_ID }
//         - { name: CLIENT_SECRET,        secret: GOOGLE_CLIENT_SECRET }
//         - { name: GOOGLE_ACCESS_TOKEN,  secret: GOOGLE_ACCESS_TOKEN,  optional: true }
//         - { name: GOOGLE_REFRESH_TOKEN, secret: GOOGLE_REFRESH_TOKEN, optional: true }

import type { DicodeSdk } from "../../sdk.ts";
import {
  buildAuthUrl, exchangeCodePKCE, exchangeCodePKCEWithSecret, exchangeCodeSecret,
  generatePKCE, handleAuthNeeded, refreshAccessTokenPKCE, refreshAccessTokenWithSecret,
  successHtml, tokenExpiresWithin,
} from "../_oauth/flow.ts";
import { resolveProvider } from "./providers.ts";

const EXPIRY_KV_SUFFIX = "_oauth_expires_at";

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const providerKey      = (await params.get("provider") ?? "").toLowerCase();
  const clientIdEnv      = await params.get("client_id_env")     ?? "CLIENT_ID";
  const clientSecretEnv  = await params.get("client_secret_env") ?? "";
  const accessTokenEnv   = await params.get("access_token_env")  ?? "ACCESS_TOKEN";
  const refreshTokenEnv  = await params.get("refresh_token_env") ?? "REFRESH_TOKEN";
  const scope            = await params.get("scope")             ?? "";
  const color            = await params.get("color")             ?? "#333";
  const tokenLifetime    = await params.get("token_lifetime")    ?? "expires";

  if (!providerKey) throw new Error("param 'provider' is required — set it via taskset overrides");

  const clientId      = Deno.env.get(clientIdEnv) ?? "";
  const clientSecret  = clientSecretEnv ? (Deno.env.get(clientSecretEnv) ?? "") : "";
  const existingToken = Deno.env.get(accessTokenEnv) ?? "";
  const existingRefresh = Deno.env.get(refreshTokenEnv) ?? "";
  const baseURL       = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");

  if (!clientId) throw new Error(
    `Client ID not set — inject it via env[name: ${clientIdEnv}, secret: YOUR_CLIENT_ID_SECRET]`
  );

  const { provider, name, redirectSuffix } = resolveProvider(providerKey);
  const redirectURI  = `${baseURL}/hooks/${redirectSuffix}`;
  const expiryKvKey  = `${providerKey}${EXPIRY_KV_SUFFIX}`;
  const verifierKvKey = `${providerKey}_oauth_verifier`;

  const inp  = input as Record<string, unknown> | null;
  const code = inp?.code as string | undefined;

  // ── Webhook leg: exchange code → store tokens ─────────────────────────────
  if (code) {
    const verifier = await kv.get(verifierKvKey) as string | null;
    if (!verifier) return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    await kv.delete(verifierKvKey);

    let tokens;
    if (clientSecret && verifier) {
      tokens = await exchangeCodePKCEWithSecret({ provider, clientId, clientSecret, redirectURI, code, verifier });
    } else if (verifier) {
      tokens = await exchangeCodePKCE({ provider, clientId, redirectURI, code, verifier });
    } else {
      tokens = await exchangeCodeSecret({ provider, clientId, clientSecret, redirectURI, code });
    }

    await dicode.secrets_set(accessTokenEnv, tokens.access_token);
    const stored = [accessTokenEnv];
    if (tokens.refresh_token) {
      await dicode.secrets_set(refreshTokenEnv, tokens.refresh_token);
      stored.push(refreshTokenEnv);
    }
    // Store extra fields providers include in their token response
    const extra = tokens as Record<string, unknown>;
    if (typeof extra.instance_url === "string") {
      const key = `${providerKey.toUpperCase()}_INSTANCE_URL`;
      await dicode.secrets_set(key, extra.instance_url);
      stored.push(key);
    }
    if (typeof extra.stripe_user_id === "string") {
      const key = `${providerKey.toUpperCase()}_ACCOUNT_ID`;
      await dicode.secrets_set(key, extra.stripe_user_id);
      stored.push(key);
    }
    if (typeof extra.workspace_id === "string") {
      const key = `${providerKey.toUpperCase()}_WORKSPACE_ID`;
      await dicode.secrets_set(key, extra.workspace_id);
      stored.push(key);
    }
    if (tokens.expires_in) await kv.set(expiryKvKey, Date.now() + tokens.expires_in * 1000);
    return output.html(successHtml(name, stored));
  }

  // ── Refresh / validity check leg ──────────────────────────────────────────
  if (existingToken) {
    if (tokenLifetime === "permanent") {
      // Token never expires (GitHub, Linear, Slack, Salesforce).
      if (inp !== null) return { valid: true };
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">${name} token already stored</h2>
        <p>This token does not expire. Re-run only to change scopes or revoke access.</p>
      </div>`);
    }

    const expiresAt = (await kv.get(expiryKvKey)) as number | null;
    if (!tokenExpiresWithin(expiresAt, 60)) {
      if (inp !== null) return { valid: true };
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">${name} token still valid</h2>
        <p>Expires at ${new Date(expiresAt!).toISOString()}.</p>
      </div>`);
    }

    if (existingRefresh) {
      try {
        const refreshed = clientSecret
          ? await refreshAccessTokenWithSecret({ provider, clientId, clientSecret, refreshToken: existingRefresh })
          : await refreshAccessTokenPKCE({ provider, clientId, refreshToken: existingRefresh });
        await dicode.secrets_set(accessTokenEnv, refreshed.access_token);
        if (refreshed.refresh_token) await dicode.secrets_set(refreshTokenEnv, refreshed.refresh_token);
        if (refreshed.expires_in) await kv.set(expiryKvKey, Date.now() + refreshed.expires_in * 1000);
        console.log(`${name} access token refreshed silently`);
        if (inp !== null) return { refreshed: true };
        return output.html(successHtml(`${name} (refreshed)`, [accessTokenEnv]));
      } catch (err) {
        console.error("Silent refresh failed:", err);
      }
    }
  }

  // ── Need (re-)authorisation ───────────────────────────────────────────────
  const { verifier, challenge } = await generatePKCE();
  await kv.set(verifierKvKey, verifier);

  const usePKCE = !!verifier; // always true; Notion/secret-only would need separate handling
  const authURL = buildAuthUrl({
    provider, clientId, redirectURI, scope,
    extra: usePKCE ? { code_challenge: challenge, code_challenge_method: "S256" } : undefined,
  });

  return handleAuthNeeded({ name, authURL, redirectURI, scope, color, isChain: inp !== null, output, dicode });
}
