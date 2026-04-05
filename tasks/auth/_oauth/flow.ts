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

// ── Token exchange: client secret (no PKCE) ───────────────────────────────────

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
