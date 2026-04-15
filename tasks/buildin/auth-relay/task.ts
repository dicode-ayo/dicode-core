// buildin/auth-relay — OAuth token delivery sink.
//
// The relay broker POSTs an OAuthTokenDeliveryPayload JSON envelope to
// /hooks/oauth-complete via the WSS tunnel. This task asks the daemon to
// decrypt the envelope, parse the token bundle, and persist the fields to
// secrets. The plaintext credentials are never returned to JS — the IPC
// call only reports which secret names were written.

export default async function main() {
  // input is the decoded webhook request body; the relay broker always
  // sends a JSON OAuthTokenDeliveryPayload, so trust the shape.
  const envelope = input;
  const result = await dicode.oauth.store_token(envelope);
  return {
    ok: true,
    provider: result.provider,
    secrets_written: result.secrets,
  };
}
