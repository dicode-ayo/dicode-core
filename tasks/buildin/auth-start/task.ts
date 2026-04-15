// buildin/auth-start — kicks off an OAuth flow via the dicode relay broker.
//
// The daemon-side dicode.oauth.build_auth_url primitive bakes the entire
// /auth/:provider payload layout in Go (BuildAuthURL in pkg/relay/oauth.go),
// so this task cannot be coaxed into signing a payload of the wrong shape.
// It only gets to pick the provider and an optional scope override.

export default async function main() {
  const provider = (await params.get("provider")) ?? "";
  const scope = (await params.get("scope")) ?? "";
  if (!provider) throw new Error("provider parameter is required");

  const result = await dicode.oauth.build_auth_url(provider, scope);

  const lines = [
    `OAuth flow started for ${result.provider}.`,
    ``,
    `Open this URL in a browser to authorize:`,
    ``,
    `  ${result.url}`,
    ``,
    `Once you complete the provider's consent screen, the dicode relay will`,
    `deliver the encrypted token to this daemon. buildin/auth-relay will`,
    `decrypt it and write the credentials to your secrets store under`,
    `${result.provider.toUpperCase()}_ACCESS_TOKEN (and _REFRESH_TOKEN, _EXPIRES_AT if applicable).`,
    ``,
    `Session: ${result.session_id}`,
  ];
  const html = `<pre>${lines.map(escapeHtml).join("\n")}</pre>`;
  await output.html(html);
  return { url: result.url, session_id: result.session_id };
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#x27;");
}
