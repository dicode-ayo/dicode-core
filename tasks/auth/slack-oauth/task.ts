import {
  authorizeHtml, buildAuthUrl, exchangeCodeSecret,
  successHtml, type OAuthProvider,
} from "../_oauth/flow.ts";

// Slack OAuth2 — Authorization Code + client secret (Slack does not support PKCE).
// See task.yaml for setup instructions.

const provider: OAuthProvider = {
  authUrl:  "https://slack.com/oauth/v2/authorize",
  tokenUrl: "https://slack.com/api/oauth.v2.access",
};

export default async function main({ params, input, output, dicode }: DicodeSdk) {
  const clientId     = Deno.env.get("SLACK_CLIENT_ID")!;
  const clientSecret = Deno.env.get("SLACK_CLIENT_SECRET")!;
  const baseURL      = (Deno.env.get("DICODE_BASE_URL") ?? "").replace(/\/$/, "");
  const redirectURI  = `${baseURL}/hooks/slack-oauth`;
  const scope        = await params.get("scope") ?? "channels:read,chat:write";
  const query        = (input as Record<string, unknown> | null)?.query as Record<string, string> | undefined;

  // ── Webhook leg: exchange code → store tokens ─────────────────────────────
  if (query?.code) {
    const tokens = await exchangeCodeSecret({ provider, clientId, clientSecret, redirectURI, code: query.code });

    // Slack v2 returns a nested structure: bot token at top level, user token nested.
    const slackResp = tokens as Record<string, unknown>;
    const stored: string[] = [];

    if (typeof slackResp.access_token === "string") {
      await dicode.secrets_set("SLACK_BOT_TOKEN", slackResp.access_token);
      stored.push("SLACK_BOT_TOKEN");
    }

    const authedUser = slackResp.authed_user as Record<string, unknown> | undefined;
    if (typeof authedUser?.access_token === "string") {
      await dicode.secrets_set("SLACK_USER_TOKEN", authedUser.access_token);
      stored.push("SLACK_USER_TOKEN");
    }

    return output.html(successHtml("Slack", stored));
  }

  // ── Manual leg: render auth URL ───────────────────────────────────────────
  const authURL = buildAuthUrl({ provider, clientId, redirectURI, scope });

  return output.html(authorizeHtml({ name: "Slack", authURL, redirectURI, scope, color: "#4A154B" }));
}
