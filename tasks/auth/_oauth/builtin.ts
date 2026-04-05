// Built-in OAuth2 client IDs shipped with dicode.
// These are registered as public "Desktop app" / PKCE-only clients.
// No secret is needed or stored — client_id is safe to distribute openly.
//
// Users can override any of these by setting their own PROVIDER_CLIENT_ID
// secret (dicode secret set GITHUB_CLIENT_ID <your-id>), which takes
// precedence over the builtin. This is useful to avoid shared API quota.
//
// Registered redirect URIs (all providers):
//   http://localhost:8080/hooks/<provider>-oauth
//
// ── Provider registration status ─────────────────────────────────────────────
// slack       — PKCE GA March 2026, registered ✓
// github      — PKCE GA July 2025,  registered — TODO
// spotify     — PKCE required,      registered — TODO
// linear      — PKCE GA Oct 2025,   registered — TODO
// discord     — PKCE optional,      registered — TODO
// confluence  — PKCE (Atlassian 3LO), registered — TODO
// salesforce  — PKCE (Connected App), registered — TODO

export const BUILTIN_CLIENT_IDS: Record<string, string> = {
  // Filled in as each app is registered with the provider.
  // "github":  "Ov23li...",
  // "spotify": "abc123...",
  // "linear":  "lin_oauth_...",
  // "discord": "123456789012345678",
  // "slack":   "123456789012.123456789012",
};

/**
 * Resolves the client ID for a provider.
 * Priority: user-set env var → builtin → throws with a helpful message.
 */
export function resolveClientId(providerKey: string, envVar: string): string {
  const fromEnv = Deno.env.get(envVar);
  if (fromEnv) return fromEnv;
  const builtin = BUILTIN_CLIENT_IDS[providerKey];
  if (builtin) return builtin;
  throw new Error(
    `No client ID found for ${providerKey}. ` +
    `Set it with: dicode secret set ${envVar} <your-client-id>`
  );
}
