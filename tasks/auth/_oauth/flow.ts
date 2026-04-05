// Shared OAuth2 helpers for dicode tasks.
// Inspired by @deno/kv-oauth provider pattern — provider config is a plain
// object; PKCE and token exchange are handled here; session state goes into
// dicode kv (instead of Deno KV).

export interface OAuthProvider {
  /** Authorization endpoint URL. */
  authUrl: string;
  /** Token exchange endpoint URL. */
  tokenUrl: string;
  /** Extra query params appended to the auth URL (e.g. access_type, prompt). */
  extraParams?: Record<string, string>;
}

export interface TokenResponse {
  access_token:  string;
  refresh_token?: string;
  expires_in?:   number;
  token_type?:   string;
  [key: string]: unknown;
}

// ── PKCE ─────────────────────────────────────────────────────────────────────

function base64url(buf: Uint8Array): string {
  return btoa(String.fromCharCode(...buf))
    .replace(/\+/g, "-").replace(/\//g, "_").replace(/=/g, "");
}

export async function generatePKCE(): Promise<{ verifier: string; challenge: string }> {
  const verifier = base64url(crypto.getRandomValues(new Uint8Array(32)));
  const digest   = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return { verifier, challenge: base64url(new Uint8Array(digest)) };
}

// ── Auth URL ──────────────────────────────────────────────────────────────────

export function buildAuthUrl(opts: {
  provider:    OAuthProvider;
  clientId:    string;
  redirectURI: string;
  scope:       string;
  extra?:      Record<string, string>;
}): string {
  const p = new URLSearchParams({
    client_id:     opts.clientId,
    redirect_uri:  opts.redirectURI,
    response_type: "code",
    scope:         opts.scope,
    ...opts.provider.extraParams,
    ...opts.extra,
  });
  return `${opts.provider.authUrl}?${p}`;
}

// ── Token exchange: PKCE (no client secret) ───────────────────────────────────

export async function exchangeCodePKCE(opts: {
  provider:    OAuthProvider;
  clientId:    string;
  redirectURI: string;
  code:        string;
  verifier:    string;
}): Promise<TokenResponse> {
  const res = await fetch(opts.provider.tokenUrl, {
    method:  "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type:    "authorization_code",
      client_id:     opts.clientId,
      redirect_uri:  opts.redirectURI,
      code:          opts.code,
      code_verifier: opts.verifier,
    }),
  });
  const data = await res.json() as TokenResponse;
  if (!res.ok) throw new Error(`Token exchange failed: ${data.error_description ?? data.error ?? res.status}`);
  return data;
}

// ── Token exchange: PKCE + client secret (Google Desktop app) ────────────────
// Google requires client_secret even for public Desktop app clients using PKCE.
// The secret is acknowledged as non-confidential by Google's own docs but is
// still required as a client identifier in the token exchange.

export async function exchangeCodePKCEWithSecret(opts: {
  provider:     OAuthProvider;
  clientId:     string;
  clientSecret: string;
  redirectURI:  string;
  code:         string;
  verifier:     string;
}): Promise<TokenResponse> {
  const res = await fetch(opts.provider.tokenUrl, {
    method:  "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type:    "authorization_code",
      client_id:     opts.clientId,
      client_secret: opts.clientSecret,
      redirect_uri:  opts.redirectURI,
      code:          opts.code,
      code_verifier: opts.verifier,
    }),
  });
  const data = await res.json() as TokenResponse;
  if (!res.ok) throw new Error(`Token exchange failed: ${data.error_description ?? data.error ?? res.status}`);
  return data;
}

// ── Token exchange: client secret only (no PKCE) ──────────────────────────────

export async function exchangeCodeSecret(opts: {
  provider:     OAuthProvider;
  clientId:     string;
  clientSecret: string;
  redirectURI:  string;
  code:         string;
}): Promise<TokenResponse> {
  const res = await fetch(opts.provider.tokenUrl, {
    method:  "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type:    "authorization_code",
      client_id:     opts.clientId,
      client_secret: opts.clientSecret,
      redirect_uri:  opts.redirectURI,
      code:          opts.code,
    }),
  });
  const data = await res.json() as TokenResponse;
  if (!res.ok) throw new Error(`Token exchange failed: ${data.error_description ?? data.error ?? res.status}`);
  return data;
}

// ── Token refresh: PKCE (no client secret) ───────────────────────────────────

export async function refreshAccessTokenPKCE(opts: {
  provider:     OAuthProvider;
  clientId:     string;
  refreshToken: string;
}): Promise<TokenResponse> {
  const res = await fetch(opts.provider.tokenUrl, {
    method:  "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type:    "refresh_token",
      client_id:     opts.clientId,
      refresh_token: opts.refreshToken,
    }),
  });
  const data = await res.json() as TokenResponse;
  if (!res.ok) throw new Error(`Token refresh failed: ${data.error_description ?? data.error ?? res.status}`);
  return data;
}

// ── Token refresh: PKCE + client secret ──────────────────────────────────────

export async function refreshAccessTokenWithSecret(opts: {
  provider:     OAuthProvider;
  clientId:     string;
  clientSecret: string;
  refreshToken: string;
}): Promise<TokenResponse> {
  const res = await fetch(opts.provider.tokenUrl, {
    method:  "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type:    "refresh_token",
      client_id:     opts.clientId,
      client_secret: opts.clientSecret,
      refresh_token: opts.refreshToken,
    }),
  });
  const data = await res.json() as TokenResponse;
  if (!res.ok) throw new Error(`Token refresh failed: ${data.error_description ?? data.error ?? res.status}`);
  return data;
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
  return `
<div style="font-family:system-ui,sans-serif;padding:2rem;max-width:480px">
  <h2 style="color:#1a7f37">${name} OAuth complete</h2>
  <p>Stored in secret store:</p>
  <ul>${secrets.map(s => `<li><code>${s}</code></li>`).join("")}</ul>
  <p style="color:#666;font-size:.9em">
    Use in other tasks via <code>env: [{ name: TOKEN, secret: ${secrets[0]} }]</code>
  </p>
</div>`;
}
