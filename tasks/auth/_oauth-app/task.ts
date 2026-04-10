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
  buildAuthUrl, exchangeCode, generatePKCE, generateState,
  handleAuthNeeded, refreshAccessToken, successHtml, tokenExpiresWithin,
} from "../_oauth/flow.ts";
import { resolveClientId } from "../_oauth/builtin.ts";
import { resolveProvider } from "./providers.ts";

const EXPIRY_KV_SUFFIX   = "_oauth_expires_at";
const VERIFIER_KV_SUFFIX = "_oauth_verifier";
const STATE_KV_SUFFIX    = "_oauth_state";

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

  // resolveClientId: env var → built-in app ID → throws with a helpful message.
  const clientId      = resolveClientId(providerKey, clientIdEnv);
  const clientSecret  = clientSecretEnv ? (Deno.env.get(clientSecretEnv) ?? "") : "";
  const existingToken = Deno.env.get(accessTokenEnv)   ?? "";
  const existingRefresh = Deno.env.get(refreshTokenEnv) ?? "";
  const baseURL       = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");

  const { provider, name, redirectSuffix } = resolveProvider(providerKey);
  const redirectURI   = `${baseURL}/hooks/${redirectSuffix}`;
  const expiryKvKey   = `${providerKey}${EXPIRY_KV_SUFFIX}`;
  const verifierKvKey = `${providerKey}${VERIFIER_KV_SUFFIX}`;
  const stateKvKey    = `${providerKey}${STATE_KV_SUFFIX}`;

  // input is undefined (not null) for manual/CLI runs because the IPC Response.result
  // field uses omitempty — coerce to null so that null-checks work correctly.
  const inp  = (input ?? null) as Record<string, unknown> | null;
  const code = inp?.code as string | undefined;

  // ── Webhook leg: exchange code → store tokens ─────────────────────────────
  if (code) {
    const verifier      = await kv.get(verifierKvKey) as string | null;
    const expectedState = await kv.get(stateKvKey)    as string | null;

    if (!verifier && !provider.noPKCE) {
      return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    }

    await kv.delete(verifierKvKey);
    await kv.delete(stateKvKey);

    const tokens = await exchangeCode({
      provider, clientId, clientSecret, redirectURI, code,
      returnedState: inp?.state as string | undefined,
      expectedState,
      verifier: provider.noPKCE ? undefined : (verifier ?? undefined),
    });

    // Log all token response fields (values masked) so we can trace what the provider sends.
    const extra = tokens as Record<string, unknown>;
    const debugFields = Object.entries(extra).map(([k, v]) => {
      if (k === "access_token" || k === "refresh_token") return `${k}=<set>`;
      return `${k}=${JSON.stringify(v)}`;
    });
    console.log(`[${name}] token response fields: ${debugFields.join(", ")}`);

    await dicode.secrets_set(accessTokenEnv, tokens.access_token);
    const stored = [accessTokenEnv];
    if (tokens.refresh_token) {
      await dicode.secrets_set(refreshTokenEnv, tokens.refresh_token);
      stored.push(refreshTokenEnv);
    }
    // Store extra fields some providers include in their token response.
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

    if (tokens.expires_in) {
      const expiresAt = Date.now() + Number(tokens.expires_in) * 1000;
      await kv.set(expiryKvKey, expiresAt);
      console.log(`[${name}] token expires in ${tokens.expires_in}s (at ${new Date(expiresAt).toISOString()})`);
    } else {
      console.log(`[${name}] no expires_in in response — token treated as ${tokenLifetime}`);
    }

    return output.html(successHtml(name, stored));
  }

  // ── Refresh / validity check leg ──────────────────────────────────────────
  if (existingToken) {
    console.log(`[${name}] existing token found (env: ${accessTokenEnv}), lifetime=${tokenLifetime}`);

    if (tokenLifetime === "permanent") {
      console.log(`[${name}] permanent token — skipping expiry check`);
      if (inp !== null) return { valid: true };
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">${name} token already stored</h2>
        <p>This token does not expire. Re-run only to change scopes or revoke access.</p>
      </div>`);
    }

    const expiresAt = (await kv.get(expiryKvKey)) as number | null;
    console.log(`[${name}] expiresAt=${expiresAt ? new Date(expiresAt).toISOString() : "not set"}, now=${new Date().toISOString()}`);

    if (!tokenExpiresWithin(expiresAt, 60)) {
      console.log(`[${name}] token still valid`);
      if (inp !== null) return { valid: true };
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">${name} token still valid</h2>
        <p>Expires at ${new Date(expiresAt!).toISOString()}.</p>
      </div>`);
    }

    console.log(`[${name}] token expired or expiring soon, refresh_token present=${!!existingRefresh}`);
    if (existingRefresh) {
      try {
        const refreshed = await refreshAccessToken({
          provider, clientId, clientSecret, refreshToken: existingRefresh,
        });
        const refreshDebug = Object.entries(refreshed as Record<string, unknown>)
          .map(([k, v]) => k === "access_token" || k === "refresh_token" ? `${k}=<set>` : `${k}=${JSON.stringify(v)}`)
          .join(", ");
        console.log(`[${name}] refresh response fields: ${refreshDebug}`);
        await dicode.secrets_set(accessTokenEnv, refreshed.access_token);
        if (refreshed.refresh_token) await dicode.secrets_set(refreshTokenEnv, refreshed.refresh_token);
        if (refreshed.expires_in) {
          const newExpiry = Date.now() + Number(refreshed.expires_in) * 1000;
          await kv.set(expiryKvKey, newExpiry);
          console.log(`[${name}] refreshed — new expiry: ${new Date(newExpiry).toISOString()}`);
        }
        if (inp !== null) return { refreshed: true };
        return output.html(successHtml(`${name} (refreshed)`, [accessTokenEnv]));
      } catch (err) {
        console.error(`[${name}] silent refresh failed:`, err);
      }
    }
  } else {
    console.log(`[${name}] no existing token (env: ${accessTokenEnv})`);
  }

  // ── Need (re-)authorisation ───────────────────────────────────────────────
  const state  = generateState();
  const pkce   = provider.noPKCE ? null : await generatePKCE();

  await kv.set(stateKvKey, state);
  if (pkce) await kv.set(verifierKvKey, pkce.verifier);

  const authURL = buildAuthUrl({
    provider, clientId, redirectURI, scope, state,
    challenge: pkce?.challenge,
  });

  return handleAuthNeeded({ name, authURL, redirectURI, scope, color, isChain: inp !== null, output, dicode });
}
