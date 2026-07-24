// Package webhooksign is the single source of truth for the HMAC-SHA256
// scheme dicode uses to authenticate webhook requests. It is shared by the
// verifier in pkg/trigger/webhook.go and by the `dicode webhook sign` CLI
// command (cmd/dicode/webhook.go), so the two can never drift apart.
//
// Scheme: hex(HMAC-SHA256(secret, preimage)), where preimage is
// "<unix_ts>\n<body>" when a timestamp is present, or the bare body
// otherwise. The digest is carried in the X-Hub-Signature-256 header as
// "sha256=<hex>"; the timestamp (when present) is carried in
// X-Dicode-Timestamp.
package webhooksign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

const (
	// SignatureHeader carries the HMAC signature, GitHub-compatible.
	SignatureHeader = "X-Hub-Signature-256"
	// TimestampHeader carries the Unix timestamp used for replay protection
	// and, when present, folded into the HMAC preimage.
	TimestampHeader = "X-Dicode-Timestamp"
)

// PreimageDigest computes hex(HMAC-SHA256(secret, preimage)), where preimage
// is "<tsStr>\n<body>" when tsStr is non-empty, or bare body otherwise. This
// is the single preimage construction shared by signature verification, the
// replay-cache key, and the `dicode webhook sign` CLI command.
func PreimageDigest(secret, tsStr string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	if tsStr != "" {
		mac.Write([]byte(tsStr))
		mac.Write([]byte("\n"))
	}
	mac.Write(body)
	return mac.Sum(nil)
}

// SignatureValue formats a digest as the value expected in the
// X-Hub-Signature-256 header: "sha256=<hex>".
func SignatureValue(digest []byte) string {
	return "sha256=" + hex.EncodeToString(digest)
}
