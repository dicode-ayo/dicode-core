import type { DicodeSdk } from "../../sdk.ts";
import {
  buildAuthUrl, exchangeCodePKCE, generatePKCE, handleAuthNeeded, successHtml,
} from "../_oauth/flow.ts";
import { Salesforce as provider } from "../_oauth/providers.ts";
import { resolveClientId } from "../_oauth/builtin.ts";

// Salesforce Connected App — PKCE only, no client secret required when
// "Enable PKCE Code Verifier" is checked in Connected App OAuth Settings.
// Tokens don't expire by default. instance_url is stored for API calls
// (Salesforce orgs have unique API hosts, e.g. na1.salesforce.com).
// For Sandbox, set SALESFORCE_AUTH_BASE to "https://test.salesforce.com".

export default async function main({ params, input, output, kv, dicode }: DicodeSdk) {
  const clientId      = resolveClientId("salesforce", "SALESFORCE_CLIENT_ID");
  const baseURL       = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const existingToken = Deno.env.get("SALESFORCE_ACCESS_TOKEN");
  const redirectURI   = `${baseURL}/hooks/salesforce-oauth`;
  const scope         = await params.get("scope") ?? "api refresh_token";
  const inp           = input as Record<string, unknown> | null;
  const code          = inp?.code as string | undefined;

  if (code) {
    const verifier = await kv.get("salesforce_oauth_verifier") as string | null;
    if (!verifier) return output.html(`<p style="color:red">PKCE verifier missing — click Run to restart.</p>`);
    await kv.delete("salesforce_oauth_verifier");
    const tokens = await exchangeCodePKCE({ provider, clientId, redirectURI, code, verifier });
    await dicode.secrets_set("SALESFORCE_ACCESS_TOKEN", tokens.access_token);
    // instance_url is org-specific — must be used as API base URL
    const instanceUrl = (tokens as Record<string, unknown>).instance_url as string | undefined;
    if (instanceUrl) await dicode.secrets_set("SALESFORCE_INSTANCE_URL", instanceUrl);
    const stored = ["SALESFORCE_ACCESS_TOKEN"];
    if (instanceUrl) stored.push("SALESFORCE_INSTANCE_URL");
    return output.html(successHtml("Salesforce", stored));
  }

  // Salesforce tokens don't expire by default — skip re-auth if token exists.
  if (existingToken) {
    if (inp !== null) return { valid: true };
    return output.html(`<div style="font-family:system-ui,sans-serif;padding:2rem">
      <h2 style="color:#1a7f37">Salesforce token already stored</h2>
      <p>Salesforce access tokens do not expire by default. Re-run to change scopes or revoke.</p>
    </div>`);
  }

  const { verifier, challenge } = await generatePKCE();
  await kv.set("salesforce_oauth_verifier", verifier);
  const authURL = buildAuthUrl({ provider, clientId, redirectURI, scope,
    extra: { code_challenge: challenge, code_challenge_method: "S256" } });

  return handleAuthNeeded({
    name: "Salesforce", authURL, redirectURI, scope, color: "#00A1E0",
    isChain: inp !== null, output, dicode,
  });
}
