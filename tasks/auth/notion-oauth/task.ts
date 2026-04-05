import type { DicodeSdk } from "../../sdk.ts";
import { buildAuthUrl, exchangeCodeSecret, handleAuthNeeded, successHtml } from "../_oauth/flow.ts";
import { Notion as provider } from "../_oauth/providers.ts";

export default async function main({ input, output, dicode }: DicodeSdk) {
  const clientId     = Deno.env.get("NOTION_CLIENT_ID")!;
  const clientSecret = Deno.env.get("NOTION_CLIENT_SECRET")!;
  const baseURL      = (Deno.env.get("DICODE_BASE_URL") ?? "http://localhost:8080").replace(/\/$/, "");
  const redirectURI  = `${baseURL}/hooks/notion-oauth`;
  const inp          = input as Record<string, unknown> | null;
  const code         = inp?.code as string | undefined;

  if (code) {
    const tokens = await exchangeCodeSecret({ provider, clientId, clientSecret, redirectURI, code });
    const resp = tokens as Record<string, unknown>;
    await dicode.secrets_set("NOTION_ACCESS_TOKEN", tokens.access_token);
    if (typeof resp.workspace_id === "string") await dicode.secrets_set("NOTION_WORKSPACE_ID", resp.workspace_id);
    return output.html(successHtml("Notion", ["NOTION_ACCESS_TOKEN", "NOTION_WORKSPACE_ID"]));
  }

  // Notion requires owner=user for user-level access; scope is fixed by the integration
  const authURL = buildAuthUrl({ provider, clientId, redirectURI, scope: "",
    extra: { owner: "user", response_type: "code" } });
  return handleAuthNeeded({
    name: "Notion", authURL, redirectURI, scope: "workspace access", color: "#000000",
    isChain: inp !== null, output, dicode,
  });
}
