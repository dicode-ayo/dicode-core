// Minimal webhook example with HMAC auth.
//
// Setup:
//   1. Store a shared secret:
//        dicode secret set HELLO_WEBHOOK_SECRET mysecret
//
//   2. Trigger with curl (signature computed over the request body):
//        BODY='{"from":"curl"}'
//        SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac mysecret | awk '{print "sha256="$2}')
//        curl -X POST http://localhost:8080/hooks/hello \
//             -H "Content-Type: application/json" \
//             -H "X-Hub-Signature-256: $SIG" \
//             -d "$BODY"
//
// Requests without a valid signature are rejected with 403 before this
// script runs — nothing here needs to verify auth.

const body = params.input as Record<string, unknown> | null;

await log.info("hello from protected webhook", { body });

return output.html(`
<div style="font-family:system-ui,sans-serif;padding:2rem">
  <h2>Hello from protected webhook</h2>
  <p>The HMAC signature was verified by dicode before this script ran.</p>
  ${body
    ? `<pre style="background:#f4f4f4;padding:1rem;border-radius:6px">${JSON.stringify(body, null, 2)}</pre>`
    : "<p><em>No body received.</em></p>"
  }
</div>
`);
