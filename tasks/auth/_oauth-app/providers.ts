// Provider registry for the generic OAuth task.
// Maps a lowercase provider key to its OAuthProvider config, display name,
// and the webhook suffix used in the redirect URI.
//
// Add new providers here; no changes to task.ts are needed.

import type { OAuthProvider } from "../_oauth/flow.ts";
import * as P from "../_oauth/providers.ts";

interface ProviderEntry {
  provider:       OAuthProvider;
  name:           string; // human-readable, used in UI and log messages
  redirectSuffix: string; // dicode webhook path suffix, e.g. "google-oauth" → /hooks/google-oauth
}

const PROVIDERS: Record<string, ProviderEntry> = {
  google: {
    provider:       P.Google,
    name:           "Google",
    redirectSuffix: "google-oauth",
  },
  slack: {
    provider:       P.Slack,
    name:           "Slack",
    redirectSuffix: "slack-oauth",
  },
  github: {
    provider:       P.GitHub,
    name:           "GitHub",
    redirectSuffix: "github-oauth",
  },
  spotify: {
    provider:       P.Spotify,
    name:           "Spotify",
    redirectSuffix: "spotify-oauth",
  },
  linear: {
    provider:       P.Linear,
    name:           "Linear",
    redirectSuffix: "linear-oauth",
  },
  discord: {
    provider:       P.Discord,
    name:           "Discord",
    redirectSuffix: "discord-oauth",
  },
  airtable: {
    provider:       P.Airtable,
    name:           "Airtable",
    redirectSuffix: "airtable-oauth",
  },
  notion: {
    provider:       P.Notion,
    name:           "Notion",
    redirectSuffix: "notion-oauth",
  },
  gitlab: {
    provider:       P.GitLab,
    name:           "GitLab",
    redirectSuffix: "gitlab-oauth",
  },
  confluence: {
    provider:       P.Confluence,
    name:           "Atlassian",
    redirectSuffix: "confluence-oauth",
  },
  salesforce: {
    provider:       P.Salesforce,
    name:           "Salesforce",
    redirectSuffix: "salesforce-oauth",
  },
  stripe: {
    provider:       P.Stripe,
    name:           "Stripe",
    redirectSuffix: "stripe-oauth",
  },
  office365: {
    provider:       P.Office365,
    name:           "Microsoft",
    redirectSuffix: "office365-oauth",
  },
  // Azure AD is tenant-specific; default to "common".
  // Override the provider in a custom entry if you need a specific tenant.
  azure: {
    provider:       P.AzureAD("common"),
    name:           "Microsoft Azure",
    redirectSuffix: "azure-oauth",
  },
};

export function resolveProvider(key: string): ProviderEntry {
  if (key === "looker") {
    const instance = (Deno.env.get("LOOKER_INSTANCE") ?? "").replace(/^https?:\/\//, "");
    if (!instance) throw new Error("Set LOOKER_INSTANCE secret (e.g. company.cloud.looker.com)");
    return {
      provider:       { authUrl: `https://${instance}/auth`, tokenUrl: `https://${instance}/api/token` },
      name:           "Looker",
      redirectSuffix: "looker-oauth",
    };
  }
  const entry = PROVIDERS[key];
  if (!entry) throw new Error(
    `Unknown OAuth provider "${key}". Known providers: ${Object.keys(PROVIDERS).join(", ")}.`
  );
  return entry;
}
