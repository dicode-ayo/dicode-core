import type { DicodeSdk } from "../../sdk.ts";
import { buildAuthUrl, exchangeCodePKCEWithSecret, generatePKCE, handleAuthNeeded, successHtml } from "../_oauth/flow.ts";
import { GitLab as provider } from "../_oauth/providers.ts";

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const clientId     = Deno.env.get("GITLAB_CLIENT_ID")!;
  const clientSecret = Deno.env.get("GITLAB_CLIENT_SECRET")!;
  const baseURL      = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const redirectURI  = `${baseURL}/hooks/gitlab-oauth`;
  const scope        = await params.get("scope") ?? "read_user read_api";
  const inp          = input as Record<string, unknown> | null;
  const code         = inp?.code as string | undefined;

  if (code) {
    const verifier = await kv.get("gitlab_oauth_verifier") as string | null;
    if (!verifier) return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    await kv.delete("gitlab_oauth_verifier");
    const tokens = await exchangeCodePKCEWithSecret({ provider, clientId, clientSecret, redirectURI, code, verifier });
    await dicode.secrets_set("GITLAB_ACCESS_TOKEN", tokens.access_token);
    if (tokens.refresh_token) await dicode.secrets_set("GITLAB_REFRESH_TOKEN", tokens.refresh_token);
    return output.html(successHtml("GitLab", ["GITLAB_ACCESS_TOKEN", "GITLAB_REFRESH_TOKEN"]));
  }

  const { verifier, challenge } = await generatePKCE();
  await kv.set("gitlab_oauth_verifier", verifier);
  const authURL = buildAuthUrl({ provider, clientId, redirectURI, scope,
    extra: { code_challenge: challenge, code_challenge_method: "S256" } });
  return handleAuthNeeded({
    name: "GitLab", authURL, redirectURI, scope, color: "#FC6D26",
    isChain: inp !== null, output, dicode,
  });
}
