import type { DicodeSdk } from "../../sdk.ts";
import {
  authorizeHtml, buildAuthUrl, exchangeCodePKCE,
  generatePKCE, successHtml, type OAuthProvider,
} from "../_oauth/flow.ts";

// Google OAuth2 — Authorization Code + PKCE (no client secret required).
// See task.yaml for setup instructions.

const provider: OAuthProvider = {
  authUrl:     "https://accounts.google.com/o/oauth2/v2/auth",
  tokenUrl:    "https://oauth2.googleapis.com/token",
  extraParams: { access_type: "offline", prompt: "consent" },
};

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const clientId    = Deno.env.get("GOOGLE_CLIENT_ID")!;
  const baseURL     = (Deno.env.get("DICODE_BASE_URL") ?? "").replace(/\/$/, "");
  const redirectURI = `${baseURL}/hooks/google-oauth`;
  const scope       = await params.get("scope") ?? "https://www.googleapis.com/auth/gmail.readonly";
  const query       = (input as Record<string, unknown> | null)?.query as Record<string, string> | undefined;

  // ── Webhook leg: exchange code → store tokens ─────────────────────────────
  if (query?.code) {
    const verifier = await kv.get("google_oauth_verifier") as string | null;
    if (!verifier) {
      return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    }
    await kv.delete("google_oauth_verifier");

    const tokens = await exchangeCodePKCE({ provider, clientId, redirectURI, code: query.code, verifier });

    const stored: string[] = [];
    if (tokens.refresh_token) {
      await dicode.secrets_set("GOOGLE_REFRESH_TOKEN", tokens.refresh_token);
      stored.push("GOOGLE_REFRESH_TOKEN");
    }
    await dicode.secrets_set("GOOGLE_ACCESS_TOKEN", tokens.access_token);
    stored.push("GOOGLE_ACCESS_TOKEN");

    return output.html(successHtml("Google", stored));
  }

  // ── Manual leg: generate PKCE + render auth URL ───────────────────────────
  const { verifier, challenge } = await generatePKCE();
  await kv.set("google_oauth_verifier", verifier);

  const authURL = buildAuthUrl({
    provider, clientId, redirectURI, scope,
    extra: { code_challenge: challenge, code_challenge_method: "S256" },
  });

  return output.html(authorizeHtml({ name: "Google", authURL, redirectURI, scope, color: "#4285f4" }));
}
