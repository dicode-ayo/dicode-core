import type { DicodeSdk } from "../../sdk.ts";
import { buildAuthUrl, exchangeCodePKCE, generatePKCE, handleAuthNeeded, successHtml } from "../_oauth/flow.ts";
import { GitHub as provider } from "../_oauth/providers.ts";
import { resolveClientId } from "../_oauth/builtin.ts";

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const clientId      = resolveClientId("github", "GITHUB_CLIENT_ID");
  const baseURL       = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const existingToken = Deno.env.get("GITHUB_ACCESS_TOKEN");
  const redirectURI   = `${baseURL}/hooks/github-oauth`;
  const scope         = await params.get("scope") ?? "user,repo";
  const inp           = input as Record<string, unknown> | null;
  const code          = inp?.code as string | undefined;

  if (code) {
    const verifier = await kv.get("github_oauth_verifier") as string | null;
    if (!verifier) return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    await kv.delete("github_oauth_verifier");
    const tokens = await exchangeCodePKCE({ provider, clientId, redirectURI, code, verifier });
    await dicode.secrets_set("GITHUB_ACCESS_TOKEN", tokens.access_token);
    return output.html(successHtml("GitHub", ["GITHUB_ACCESS_TOKEN"]));
  }

  // GitHub OAuth tokens don't expire by default — skip re-auth if token exists.
  if (existingToken) {
    if (inp !== null) return { valid: true };
    return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
      <h2 style="color:#1a7f37">GitHub token already stored</h2>
      <p>Token is long-lived. Re-run only if you need to change scopes or revoke access.</p>
    </div>`);
  }

  const { verifier, challenge } = await generatePKCE();
  await kv.set("github_oauth_verifier", verifier);
  const authURL = buildAuthUrl({ provider, clientId, redirectURI, scope,
    extra: { code_challenge: challenge, code_challenge_method: "S256" } });

  return handleAuthNeeded({
    name: "GitHub", authURL, redirectURI, scope, color: "#24292e",
    isChain: inp !== null, output, dicode,
  });
}
