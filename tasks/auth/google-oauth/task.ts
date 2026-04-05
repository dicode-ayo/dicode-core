import type { DicodeSdk } from "../../sdk.ts";
import {
  buildAuthUrl, exchangeCodePKCEWithSecret, generatePKCE,
  handleAuthNeeded, refreshAccessTokenWithSecret, successHtml,
  tokenExpiresWithin, type OAuthProvider,
} from "../_oauth/flow.ts";

// Google OAuth2 — Authorization Code + PKCE + client secret.
// Google requires client_secret even for Desktop app + PKCE flows.
// The secret is acknowledged as non-confidential by Google's own docs
// ("the client secret is obviously not treated as a secret") but is still
// required as a client identifier. PKCE provides the real security.
// See task.yaml for setup instructions.

const provider: OAuthProvider = {
  authUrl:     "https://accounts.google.com/o/oauth2/v2/auth",
  tokenUrl:    "https://oauth2.googleapis.com/token",
  extraParams: { access_type: "offline", prompt: "consent" },
};

const EXPIRY_KV_KEY = "google_oauth_expires_at";

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const clientId        = Deno.env.get("GOOGLE_CLIENT_ID")!;
  const clientSecret    = Deno.env.get("GOOGLE_CLIENT_SECRET")!;
  const baseURL         = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const existingToken   = Deno.env.get("GOOGLE_ACCESS_TOKEN");
  const existingRefresh = Deno.env.get("GOOGLE_REFRESH_TOKEN");
  const redirectURI     = `${baseURL}/hooks/google-oauth`;
  const scope           = await params.get("scope") ?? "https://www.googleapis.com/auth/gmail.readonly";

  // GET /hooks/google-oauth?code=...&scope=... → input is the flat query param map
  const inp  = input as Record<string, unknown> | null;
  const code = inp?.code as string | undefined;

  // ── Webhook leg: exchange code → store tokens ─────────────────────────────
  if (code) {
    const verifier = await kv.get("google_oauth_verifier") as string | null;
    if (!verifier) {
      return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    }
    await kv.delete("google_oauth_verifier");

    const tokens = await exchangeCodePKCEWithSecret({
      provider, clientId, clientSecret, redirectURI, code, verifier,
    });
    await storeTokens(tokens, kv, dicode);
    const stored = ["GOOGLE_ACCESS_TOKEN"];
    if (tokens.refresh_token) stored.unshift("GOOGLE_REFRESH_TOKEN");
    return output.html(successHtml("Google", stored));
  }

  // ── Refresh / re-auth leg ─────────────────────────────────────────────────
  if (existingToken) {
    const expiresAt = (await kv.get(EXPIRY_KV_KEY)) as number | null;

    // Token is still fresh — nothing to do.
    if (!tokenExpiresWithin(expiresAt, 60)) {
      return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
        <h2 style="color:#1a7f37">Google token still valid</h2>
        <p>Access token expires at ${new Date(expiresAt!).toISOString()}.</p>
      </div>`);
    }

    // Try silent refresh.
    if (existingRefresh) {
      try {
        const tokens = await refreshAccessTokenWithSecret({
          provider, clientId, clientSecret, refreshToken: existingRefresh,
        });
        await storeTokens(tokens, kv, dicode);
        console.log("Google access token refreshed silently");
        // Return JSON for chain callers; HTML for interactive callers.
        if (inp !== null) return { refreshed: true };
        return output.html(successHtml("Google (refreshed)", ["GOOGLE_ACCESS_TOKEN"]));
      } catch (err) {
        console.error("Silent refresh failed, falling back to re-auth:", err);
        // Fall through to re-auth.
      }
    }
  }

  // ── Need (re-)authorisation ───────────────────────────────────────────────
  const { verifier, challenge } = await generatePKCE();
  await kv.set("google_oauth_verifier", verifier);
  const authURL = buildAuthUrl({
    provider, clientId, redirectURI, scope,
    extra: { code_challenge: challenge, code_challenge_method: "S256" },
  });

  return handleAuthNeeded({
    name: "Google", authURL, redirectURI, scope, color: "#4285f4",
    isChain: inp !== null, output, dicode,
  });
}

async function storeTokens(
  tokens: { access_token: string; refresh_token?: string; expires_in?: number },
  kv: DicodeSdk["kv"],
  dicode: DicodeSdk["dicode"],
) {
  await dicode.secrets_set("GOOGLE_ACCESS_TOKEN", tokens.access_token);
  if (tokens.refresh_token) await dicode.secrets_set("GOOGLE_REFRESH_TOKEN", tokens.refresh_token);
  if (tokens.expires_in) {
    await kv.set(EXPIRY_KV_KEY, Date.now() + tokens.expires_in * 1000);
  }
}
