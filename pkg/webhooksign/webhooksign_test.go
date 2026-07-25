package webhooksign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// referenceDigest is an independent re-implementation of the HMAC scheme
// (not calling PreimageDigest) so the tests actually pin the wire format
// rather than checking the function against itself.
func referenceDigest(secret, tsStr string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	if tsStr != "" {
		mac.Write([]byte(tsStr))
		mac.Write([]byte("\n"))
	}
	mac.Write(body)
	return mac.Sum(nil)
}

func TestPreimageDigest_WithTimestamp(t *testing.T) {
	secret := "s3cr3t"
	ts := "1700000000"
	body := []byte(`{"hello":"world"}`)

	got := PreimageDigest(secret, ts, body)
	want := referenceDigest(secret, ts, body)

	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("digest mismatch: got %x want %x", got, want)
	}
}

func TestPreimageDigest_WithoutTimestamp(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"hello":"world"}`)

	got := PreimageDigest(secret, "", body)
	want := referenceDigest(secret, "", body)

	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("digest mismatch: got %x want %x", got, want)
	}

	// Sanity: the bare-body preimage must NOT equal HMAC(secret, "\n"+body) —
	// i.e. an empty timestamp string must behave identically to "no timestamp
	// at all", not "timestamp is the empty string".
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("\n"))
	mac.Write(body)
	wrongDigest := mac.Sum(nil)
	if hex.EncodeToString(got) == hex.EncodeToString(wrongDigest) {
		t.Fatalf("bare-body digest unexpectedly matches the ts=\"\" + newline-prefixed variant")
	}
}

func TestSignatureValue_Format(t *testing.T) {
	digest := PreimageDigest("secret", "", []byte("body"))
	got := SignatureValue(digest)
	want := "sha256=" + hex.EncodeToString(digest)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if len(got) <= len("sha256=") {
		t.Fatalf("signature value looks too short: %q", got)
	}
}

func TestPreimageDigest_DifferentInputsDifferentDigests(t *testing.T) {
	base := PreimageDigest("secret-a", "1700000000", []byte("body-1"))

	cases := map[string][]byte{
		"different secret":    PreimageDigest("secret-b", "1700000000", []byte("body-1")),
		"different timestamp": PreimageDigest("secret-a", "1700000001", []byte("body-1")),
		"different body":      PreimageDigest("secret-a", "1700000000", []byte("body-2")),
		"no timestamp":        PreimageDigest("secret-a", "", []byte("body-1")),
	}

	baseHex := hex.EncodeToString(base)
	for name, digest := range cases {
		if hex.EncodeToString(digest) == baseHex {
			t.Errorf("%s: digest unexpectedly matches base digest", name)
		}
	}

	// And distinct cases must differ from each other too (not just from base).
	seen := map[string]string{}
	for name, digest := range cases {
		h := hex.EncodeToString(digest)
		if other, ok := seen[h]; ok {
			t.Errorf("%s and %s produced the same digest", name, other)
		}
		seen[h] = name
	}
}
