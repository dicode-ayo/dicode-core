// Shared OAuth2 helpers for dicode tasks.
// Uses oauth4webapi for PKCE, token exchange, and response parsing.
// Provider config is a plain AuthorizationServer object (no discovery required).

import * as oauth from "jsr:@panva/oauth4webapi";

export { oauth };

// Extend AuthorizationServer with provider convenience fields.
export type OAuthProvider = oauth.AuthorizationServer & {
  /** Extra query params appended to the auth URL (e.g. access_type, prompt). */
  extraParams?: Record<string, string>;
  /** Set true for providers that don't support PKCE (e.g. Notion, Stripe). */
  noPKCE?: boolean;
  /** Client authentication method for token endpoint. Default: "post". */
  clientAuthMethod?: "post" | "basic";
};

export interface TokenResponse {
  access_token:  string;
  refresh_token?: string;
  expires_in?:   number;
  token_type?:   string;
  [key: string]: unknown;
}

// ── PKCE & state ─────────────────────────────────────────────────────────────

export async function generatePKCE(): Promise<{ verifier: string; challenge: string }> {
  const verifier = oauth.generateRandomCodeVerifier();
  const challenge = await oauth.calculatePKCECodeChallenge(verifier);
  return { verifier, challenge };
}

export function generateState(): string {
  return oauth.generateRandomState();
}

// ── Auth URL ──────────────────────────────────────────────────────────────────

export function buildAuthUrl(opts: {
  provider:    OAuthProvider;
  clientId:    string;
  redirectURI: string;
  scope:       string;
  state:       string;
  challenge?:  string; // omit for noPKCE providers
  extra?:      Record<string, string>;
}): string {
  const url = new URL(opts.provider.authorization_endpoint!);
  url.searchParams.set("client_id",     opts.clientId);
  url.searchParams.set("redirect_uri",  opts.redirectURI);
  url.searchParams.set("response_type", "code");
  url.searchParams.set("scope",         opts.scope);
  url.searchParams.set("state",         opts.state);
  if (opts.challenge) {
    url.searchParams.set("code_challenge",        opts.challenge);
    url.searchParams.set("code_challenge_method", "S256");
  }
  for (const [k, v] of Object.entries(opts.provider.extraParams ?? {})) url.searchParams.set(k, v);
  for (const [k, v] of Object.entries(opts.extra ?? {})) url.searchParams.set(k, v);
  return url.toString();
}

// ── Token exchange ────────────────────────────────────────────────────────────

function makeClientAuth(provider: OAuthProvider, secret: string): oauth.ClientAuth {
  if (!secret) return oauth.None();
  return provider.clientAuthMethod === "basic"
    ? oauth.ClientSecretBasic(secret)
    : oauth.ClientSecretPost(secret);
}

export async function exchangeCode(opts: {
  provider:      OAuthProvider;
  clientId:      string;
  clientSecret:  string; // empty = public PKCE-only client
  redirectURI:   string;
  code:          string;
  returnedState?: string;
  expectedState?: string | null;
  verifier?:     string; // omit for noPKCE providers
}): Promise<TokenResponse> {
  const as     = opts.provider as oauth.AuthorizationServer;
  const client = { client_id: opts.clientId } satisfies oauth.Client;

  // Reconstruct callback URL so validateAuthResponse can parse code + state.
  const callbackUrl = new URL(opts.redirectURI);
  callbackUrl.searchParams.set("code", opts.code);
  if (opts.returnedState) callbackUrl.searchParams.set("state", opts.returnedState);

  const params = oauth.validateAuthResponse(
    as, client, callbackUrl,
    opts.expectedState ?? oauth.skipStateCheck,
  );

  const response = await oauth.authorizationCodeGrantRequest(
    as, client,
    makeClientAuth(opts.provider, opts.clientSecret),
    params, opts.redirectURI,
    opts.verifier ?? oauth.nopkce,
  );

  const result = await oauth.processAuthorizationCodeResponse(as, client, response);
  return result as unknown as TokenResponse;
}

// ── Token refresh ─────────────────────────────────────────────────────────────

export async function refreshAccessToken(opts: {
  provider:     OAuthProvider;
  clientId:     string;
  clientSecret: string; // empty = public PKCE-only client
  refreshToken: string;
}): Promise<TokenResponse> {
  const as     = opts.provider as oauth.AuthorizationServer;
  const client = { client_id: opts.clientId } satisfies oauth.Client;

  const response = await oauth.refreshTokenGrantRequest(
    as, client,
    makeClientAuth(opts.provider, opts.clientSecret),
    opts.refreshToken,
  );

  const result = await oauth.processRefreshTokenResponse(as, client, response);
  return result as unknown as TokenResponse;
}

// ── Expiry helpers ────────────────────────────────────────────────────────────

/** Returns true if the token expires within `seconds` from now (or expiry unknown). */
export function tokenExpiresWithin(expiresAt: number | null | undefined, seconds: number): boolean {
  if (expiresAt == null) return true;
  return Date.now() >= (expiresAt - seconds * 1000);
}

// ── Auth entry point: log URL + HTML or chain notification ───────────────────
//
// Call this at the end of every auth task instead of duplicating the pattern.
// It always logs the authorization URL (visible in CLI, CI logs, and run detail),
// then either returns the HTML page (manual/webhook) or fires a desktop
// notification and throws (chain/cron trigger — no interactive output available).
//
// `isChain` should be true whenever `input !== null && !code` (non-interactive run).

export async function handleAuthNeeded(opts: {
  name:        string;
  authURL:     string;
  redirectURI: string;
  scope:       string;
  color?:      string;
  isChain:     boolean;
  output:      { html(content: string): unknown };
  dicode:      { run_task(id: string, params?: Record<string, string>): Promise<unknown> };
}): Promise<unknown> {
  // Always log — shows up in `dicode run`, CI job logs, and the run-detail log panel.
  console.log(`\n  ${opts.name} OAuth — open this URL to authorize:\n  ${opts.authURL}\n`);

  if (opts.isChain) {
    // Non-interactive: send a desktop notification and fail fast with the URL in the error.
    try {
      await opts.dicode.run_task("buildin/notifications", {
        title:    `${opts.name} re-authorisation required`,
        body:     `Open dicode and run "${opts.name} OAuth" to refresh your access token.`,
        priority: "high",
      });
    } catch (_) { /* notification is best-effort */ }
    throw new Error(
      `${opts.name} OAuth token expired or missing — re-auth required.\n` +
      `Open this URL to authorize: ${opts.authURL}`
    );
  }

  return opts.output.html(authorizeHtml({
    name: opts.name, authURL: opts.authURL,
    redirectURI: opts.redirectURI, scope: opts.scope, color: opts.color,
  }));
}

// ── Shared UI helpers ─────────────────────────────────────────────────────────

export function authorizeHtml(opts: {
  name:        string;
  authURL:     string;
  redirectURI: string;
  scope:       string;
  color?:      string;
}): string {
  return `
<div style="font-family:system-ui,sans-serif;padding:2rem;max-width:540px">
  <h2>Authorize ${opts.name}</h2>
  <p><strong>Redirect URI:</strong> <code>${opts.redirectURI}</code></p>
  <p><strong>Scope:</strong> <code>${opts.scope}</code></p>
  <a href="${opts.authURL}" style="display:inline-block;padding:.75rem 1.5rem;
    background:${opts.color ?? "#333"};color:#fff;border-radius:6px;
    text-decoration:none;font-weight:600;margin-top:1rem">
    Authorize with ${opts.name}
  </a>
</div>`;
}

export function successHtml(name: string, secrets: string[]): string {
  // Peer tabs waiting on this auth (chat UI, tool UIs) get notified via the
  // /dicode-oauth-broadcast.js helper, which fires a BroadcastChannel
  // message and an optional window.opener.postMessage. The helper must be
  // loaded as an external script — the webui's CSP blocks inline <script>,
  // so an inline version silently fails.
  const keysParam = encodeURIComponent(secrets.join(","));
  return `
<div style="font-family:system-ui,sans-serif;padding:2rem;max-width:480px">
  <h2 style="color:#1a7f37">${name} OAuth complete</h2>
  <p>Stored in secret store:</p>
  <ul>${secrets.map(s => `<li><code>${s}</code></li>`).join("")}</ul>
  <p style="color:#666;font-size:.9em">
    Use in other tasks via <code>env: [{ name: TOKEN, secret: ${secrets[0]} }]</code>
  </p>
  <p style="color:#666;font-size:.9em">You can close this tab.</p>
</div>
<script src="/dicode-oauth-broadcast.js?keys=${keysParam}" defer></script>`;
}
