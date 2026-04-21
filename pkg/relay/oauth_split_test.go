package relay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/url"
	"testing"

	"golang.org/x/crypto/hkdf"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u
}

// ecdsaVerify parses a 65-byte uncompressed P-256 pubkey and checks sig.
func ecdsaVerify(uncompressedPub, digest, sig []byte) bool {
	pub, err := unmarshalUncompressed(uncompressedPub)
	if err != nil {
		return false
	}
	return ecdsa.VerifyASN1(pub, digest, sig)
}

// TestOAuth_UsesDecryptKey is the positive/negative canary for the split:
// encrypting under the SignKey pubkey must fail to decrypt (because the
// daemon now opens with DecryptKey), while encrypting under the DecryptKey
// pubkey must succeed. Accidentally re-wiring DecryptOAuthToken back to
// identity.SignKey — or teaching the broker to encrypt under the SignKey
// pubkey — would flip either half of this assertion.
func TestOAuth_UsesDecryptKey(t *testing.T) {
	id := NewTestIdentity(t)
	plaintext := []byte(`{"access_token":"xoxb-test"}`)
	sessionID := "550e8400-e29b-41d4-a716-446655440000"

	t.Run("encrypted to SignKey pubkey fails to decrypt", func(t *testing.T) {
		payload := encryptTo(t, id.SignPublicKey(), sessionID, plaintext)
		if _, err := DecryptOAuthToken(id, payload); err == nil {
			t.Fatal("expected decrypt to fail when broker encrypted under the SignKey pubkey; " +
				"this is the whole point of the #104 split — if this succeeds, DecryptOAuthToken " +
				"is still using the sign key or the broker is encrypting to the wrong key")
		}
	})

	t.Run("encrypted to DecryptKey pubkey succeeds", func(t *testing.T) {
		payload := encryptTo(t, id.DecryptPublicKey(), sessionID, plaintext)
		got, err := DecryptOAuthToken(id, payload)
		if err != nil {
			t.Fatalf("decrypt with DecryptKey should succeed: %v", err)
		}
		if string(got) != string(plaintext) {
			t.Fatalf("plaintext mismatch: got %q want %q", got, plaintext)
		}
	})
}

// TestAuthURL_UsesSignKey confirms that BuildAuthURL signs with the SignKey,
// not the DecryptKey. We verify the signature against id.SignKey.PublicKey.
// If BuildAuthURL were wired to DecryptKey the verification would fail even
// though the URL fields are all well-formed.
func TestAuthURL_UsesSignKey(t *testing.T) {
	id := NewTestIdentity(t)

	rawURL, _, err := BuildAuthURL("https://relay.dicode.app", id, "slack", "", 1_700_000_000)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Parse out session/challenge/sig (minimal parsing — we own the format).
	// BuildAuthURL's own round-trip test already exercises the verify path;
	// here we re-verify explicitly with id.SignKey to lock in the invariant.
	u := mustParseURL(t, rawURL)
	payload, err := BuildAuthSignedPayload(
		u.Query().Get("session"),
		u.Query().Get("challenge"),
		u.Query().Get("relay_uuid"),
		"slack",
		1_700_000_000,
	)
	if err != nil {
		t.Fatalf("rebuild payload: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(u.Query().Get("sig"))
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if !ecdsaVerify(id.SignPublicKey(), payload, sig) {
		t.Fatal("URL signature does not verify under SignKey — BuildAuthURL may be signing with DecryptKey")
	}
	// And crucially: it must NOT verify under DecryptKey. This is the
	// distinctness invariant on the sign side of the split.
	if ecdsaVerify(id.DecryptPublicKey(), payload, sig) {
		t.Fatal("URL signature unexpectedly verifies under DecryptKey — SignKey and DecryptKey may be aliased")
	}
}

// TestWSS_UsesSignKey verifies the handshake code path only touches SignKey:
// the daemon must be able to connect with DecryptKey left nil, and must
// refuse to sign the handshake if SignKey is nil.
func TestWSS_UsesSignKey(t *testing.T) {
	t.Run("signChallenge succeeds with only SignKey populated", func(t *testing.T) {
		id := NewTestIdentity(t)
		id.DecryptKey = nil // prove the handshake doesn't read DecryptKey
		nonce := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			t.Fatalf("rand: %v", err)
		}
		if _, err := signChallenge(id.SignKey, nonce, 1_700_000_000); err != nil {
			t.Fatalf("signChallenge should not need DecryptKey: %v", err)
		}
		// And the SignPublicKey accessor must still work.
		if got := id.SignPublicKey(); len(got) != 65 {
			t.Fatalf("SignPublicKey len=%d", len(got))
		}
	})

	t.Run("BuildAuthURL rejects identity with nil SignKey", func(t *testing.T) {
		id := NewTestIdentity(t)
		id.SignKey = nil
		if _, _, err := BuildAuthURL("https://r", id, "slack", "", 1); err == nil {
			t.Fatal("expected BuildAuthURL to refuse an identity with nil SignKey")
		}
	})

	t.Run("DecryptOAuthToken rejects identity with nil DecryptKey", func(t *testing.T) {
		id := NewTestIdentity(t)
		id.DecryptKey = nil
		if _, err := DecryptOAuthToken(id, &OAuthTokenDeliveryPayload{
			Type: OAuthDeliveryType,
		}); err == nil {
			t.Fatal("expected DecryptOAuthToken to refuse an identity with nil DecryptKey")
		}
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// encryptTo mirrors dicode-relay's eciesEncrypt exactly, but lets the caller
// pick *which* daemon pubkey to encrypt against — so we can verify the
// daemon's Decrypt function is keyed to DecryptKey and not SignKey.
func encryptTo(t *testing.T, daemonPubUncompressed []byte, sessionID string, plaintext []byte) *OAuthTokenDeliveryPayload {
	t.Helper()
	daemonPub, err := ecdh.P256().NewPublicKey(daemonPubUncompressed)
	if err != nil {
		t.Fatalf("daemon pub: %v", err)
	}
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("eph: %v", err)
	}
	shared, err := eph.ECDH(daemonPub)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	encKey := make([]byte, 32)
	if _, err := io.ReadFull(
		hkdf.New(sha256.New, shared, []byte(sessionID), []byte(hkdfInfo)),
		encKey,
	); err != nil {
		t.Fatalf("hkdf: %v", err)
	}
	iv := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		t.Fatalf("iv: %v", err)
	}
	block, _ := aes.NewCipher(encKey)
	aead, _ := cipher.NewGCM(block)
	ct := aead.Seal(nil, iv, plaintext, []byte(OAuthDeliveryType))
	return &OAuthTokenDeliveryPayload{
		Type:            OAuthDeliveryType,
		SessionID:       sessionID,
		EphemeralPubkey: base64.StdEncoding.EncodeToString(eph.PublicKey().Bytes()),
		Ciphertext:      base64.StdEncoding.EncodeToString(ct),
		Nonce:           base64.StdEncoding.EncodeToString(iv),
	}
}
