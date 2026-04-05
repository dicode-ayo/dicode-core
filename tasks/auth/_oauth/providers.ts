// Well-known OAuth2 provider configurations.
// Import the one you need in your task — no runtime dependency, just data.
import type { OAuthProvider } from "./flow.ts";

export const Google: OAuthProvider = {
  authUrl:     "https://accounts.google.com/o/oauth2/v2/auth",
  tokenUrl:    "https://oauth2.googleapis.com/token",
  extraParams: { access_type: "offline", prompt: "consent" },
};

// PKCE only — no client_secret needed (GA March 2026).
// Note: supports user token scopes only when using localhost redirect.
export const Slack: OAuthProvider = {
  authUrl:  "https://slack.com/oauth/v2/authorize",
  tokenUrl: "https://slack.com/api/oauth.v2.access",
};

// PKCE replaces client_secret (GitHub, July 2025).
export const GitHub: OAuthProvider = {
  authUrl:  "https://github.com/login/oauth/authorize",
  tokenUrl: "https://github.com/login/oauth/access_token",
};

// PKCE required — no client_secret in token exchange.
export const Spotify: OAuthProvider = {
  authUrl:  "https://accounts.spotify.com/authorize",
  tokenUrl: "https://accounts.spotify.com/api/token",
};

// PKCE optional but replaces secret when used.
export const Linear: OAuthProvider = {
  authUrl:  "https://linear.app/oauth/authorize",
  tokenUrl: "https://api.linear.app/oauth/token",
};

// PKCE optional — replaces secret when used.
export const Discord: OAuthProvider = {
  authUrl:  "https://discord.com/api/oauth2/authorize",
  tokenUrl: "https://discord.com/api/oauth2/token",
};

// Requires client_secret + PKCE in token exchange.
export const Airtable: OAuthProvider = {
  authUrl:  "https://airtable.com/oauth2/v1/authorize",
  tokenUrl: "https://airtable.com/oauth2/v1/token",
};

// Requires client_secret. No PKCE support.
export const Notion: OAuthProvider = {
  authUrl:  "https://api.notion.com/v1/oauth/authorize",
  tokenUrl: "https://api.notion.com/v1/oauth/token",
};

// Requires client_secret + PKCE (same as Google — secret is non-confidential).
// Replace {tenant} with "common", "organizations", "consumers", or your tenant ID.
export function AzureAD(tenant = "common"): OAuthProvider {
  return {
    authUrl:  `https://login.microsoftonline.com/${tenant}/oauth2/v2.0/authorize`,
    tokenUrl: `https://login.microsoftonline.com/${tenant}/oauth2/v2.0/token`,
  };
}

// Requires client_secret + PKCE.
export const GitLab: OAuthProvider = {
  authUrl:  "https://gitlab.com/oauth/authorize",
  tokenUrl: "https://gitlab.com/oauth/token",
};

// ── New providers ─────────────────────────────────────────────────────────────

// Atlassian 3LO — PKCE, no client_secret needed for public apps (Confluence, Jira, Trello).
// Include `offline_access` in scope to receive a refresh token.
// After auth, fetch accessible cloud resources:
//   GET https://api.atlassian.com/oauth/token/accessible-resources (Bearer <token>)
export const Confluence: OAuthProvider = {
  authUrl:     "https://auth.atlassian.com/authorize",
  tokenUrl:    "https://auth.atlassian.com/oauth/token",
  extraParams: { audience: "api.atlassian.com", prompt: "consent" },
};

// Looker — PKCE. Auth and token URLs are instance-specific.
// Pass your Looker hostname (e.g. "company.cloud.looker.com").
// For self-hosted Looker, include the API port if needed (e.g. "company.looker.com:19999").
export function Looker(instance: string): OAuthProvider {
  return {
    authUrl:  `https://${instance}/auth`,
    tokenUrl: `https://${instance}/api/token`,
  };
}

// Microsoft identity platform — Office 365, Teams, OneDrive, SharePoint via Microsoft Graph.
// Pre-configured for multi-tenant Graph access; same underlying platform as AzureAD.
// Requires client_secret even with PKCE (configure Desktop app type in Azure portal).
// Add offline_access to scope to receive a refresh token.
export const Office365 = AzureAD("common");

// Salesforce Connected App — PKCE eliminates client_secret when "Use PKCE" is enabled
// in the Connected App settings. Tokens don't expire by default.
// Token response includes instance_url (org-specific API root).
// For Sandbox: change authUrl/tokenUrl base to "https://test.salesforce.com".
export const Salesforce: OAuthProvider = {
  authUrl:  "https://login.salesforce.com/services/oauth2/authorize",
  tokenUrl: "https://login.salesforce.com/services/oauth2/token",
};

// Stripe Connect — OAuth for connecting Stripe accounts to your platform.
// Requires client_secret (no PKCE support). client_id = "ca_xxx", client_secret = "sk_xxx".
// Token response includes stripe_user_id (connected account ID).
// Access tokens may be permanent for Standard accounts; refresh for safety.
export const Stripe: OAuthProvider = {
  authUrl:  "https://connect.stripe.com/oauth/authorize",
  tokenUrl: "https://connect.stripe.com/oauth/token",
};
