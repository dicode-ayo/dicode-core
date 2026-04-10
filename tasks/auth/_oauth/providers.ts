// Well-known OAuth2 provider configurations.
// Fields follow the OIDC/RFC 8414 AuthorizationServer metadata naming convention.
// Import the one you need in your task — no runtime dependency, just data.
import type { OAuthProvider } from "./flow.ts";

export const Google: OAuthProvider = {
  issuer:                  "https://accounts.google.com",
  authorization_endpoint:  "https://accounts.google.com/o/oauth2/v2/auth",
  token_endpoint:          "https://oauth2.googleapis.com/token",
  extraParams: { access_type: "offline", prompt: "consent" },
};

// PKCE only — no client_secret needed (GA March 2026).
export const Slack: OAuthProvider = {
  issuer:                 "https://slack.com",
  authorization_endpoint: "https://slack.com/oauth/v2/authorize",
  token_endpoint:         "https://slack.com/api/oauth.v2.access",
};

// PKCE supported (July 2025); client_secret still required for OAuth Apps.
export const GitHub: OAuthProvider = {
  issuer:                 "https://github.com",
  authorization_endpoint: "https://github.com/login/oauth/authorize",
  token_endpoint:         "https://github.com/login/oauth/access_token",
};

// PKCE required — no client_secret in token exchange.
export const Spotify: OAuthProvider = {
  issuer:                 "https://accounts.spotify.com",
  authorization_endpoint: "https://accounts.spotify.com/authorize",
  token_endpoint:         "https://accounts.spotify.com/api/token",
};

// PKCE optional but replaces secret when used.
export const Linear: OAuthProvider = {
  issuer:                 "https://linear.app",
  authorization_endpoint: "https://linear.app/oauth/authorize",
  token_endpoint:         "https://api.linear.app/oauth/token",
};

// PKCE optional — replaces secret when used.
export const Discord: OAuthProvider = {
  issuer:                 "https://discord.com",
  authorization_endpoint: "https://discord.com/api/oauth2/authorize",
  token_endpoint:         "https://discord.com/api/oauth2/token",
};

// Requires client_secret + PKCE in token exchange.
export const Airtable: OAuthProvider = {
  issuer:                 "https://airtable.com",
  authorization_endpoint: "https://airtable.com/oauth2/v1/authorize",
  token_endpoint:         "https://airtable.com/oauth2/v1/token",
};

// Requires client_secret + Basic auth. No PKCE support.
export const Notion: OAuthProvider = {
  issuer:                 "https://api.notion.com",
  authorization_endpoint: "https://api.notion.com/v1/oauth/authorize",
  token_endpoint:         "https://api.notion.com/v1/oauth/token",
  noPKCE:           true,
  clientAuthMethod: "basic",
};

// Requires client_secret even with PKCE (configure Desktop app type in Azure portal).
// Replace {tenant} with "common", "organizations", "consumers", or your tenant ID.
export function AzureAD(tenant = "common"): OAuthProvider {
  return {
    issuer:                 `https://login.microsoftonline.com/${tenant}/v2.0`,
    authorization_endpoint: `https://login.microsoftonline.com/${tenant}/oauth2/v2.0/authorize`,
    token_endpoint:         `https://login.microsoftonline.com/${tenant}/oauth2/v2.0/token`,
  };
}

// Requires client_secret + PKCE.
export const GitLab: OAuthProvider = {
  issuer:                 "https://gitlab.com",
  authorization_endpoint: "https://gitlab.com/oauth/authorize",
  token_endpoint:         "https://gitlab.com/oauth/token",
};

// ── New providers ─────────────────────────────────────────────────────────────

// Atlassian 3LO — PKCE, no client_secret needed for public apps (Confluence, Jira, Trello).
// Include `offline_access` in scope to receive a refresh token.
export const Confluence: OAuthProvider = {
  issuer:                 "https://auth.atlassian.com",
  authorization_endpoint: "https://auth.atlassian.com/authorize",
  token_endpoint:         "https://auth.atlassian.com/oauth/token",
  extraParams: { audience: "api.atlassian.com", prompt: "consent" },
};

// Looker — PKCE. Auth and token URLs are instance-specific.
export function Looker(instance: string): OAuthProvider {
  return {
    issuer:                 `https://${instance}`,
    authorization_endpoint: `https://${instance}/auth`,
    token_endpoint:         `https://${instance}/api/token`,
  };
}

// Microsoft identity platform — Office 365, Teams, OneDrive, SharePoint via Microsoft Graph.
export const Office365 = AzureAD("common");

// Salesforce Connected App — PKCE eliminates client_secret when "Use PKCE" is enabled.
// Token response includes instance_url (org-specific API root).
export const Salesforce: OAuthProvider = {
  issuer:                 "https://login.salesforce.com",
  authorization_endpoint: "https://login.salesforce.com/services/oauth2/authorize",
  token_endpoint:         "https://login.salesforce.com/services/oauth2/token",
};

// Stripe Connect — OAuth for connecting Stripe accounts to your platform.
// No PKCE support. client_id = "ca_xxx", client_secret = "sk_xxx".
// Token response includes stripe_user_id (connected account ID).
export const Stripe: OAuthProvider = {
  issuer:                 "https://stripe.com",
  authorization_endpoint: "https://connect.stripe.com/oauth/authorize",
  token_endpoint:         "https://connect.stripe.com/oauth/token",
  noPKCE: true,
};
