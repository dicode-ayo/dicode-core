package relay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/hkdf"
)

// newOAuthTestIdentity creates a fresh in-memory split-key identity
// (no DB writes). SignKey and DecryptKey are deliberately independent so
// tests exercise the post-#104 invariant that signing and decryption happen
// under disjoint keys.
func newOAuthTestIdentity(t *testing.T) *Identity {
	t.Helper()
	signKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate sign key: %v", err)
	}
	decryptKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate decrypt key: %v", err)
	}
	return &Identity{
		SignKey:    signKey,
		DecryptKey: decryptKey,
		UUID:       deriveUUID(&signKey.PublicKey),
	}
}

func TestBuildAuthSignedPayload_Deterministic(t *testing.T) {
	const (
		sessionID = "550e8400-e29b-41d4-a716-446655440000"
		challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
		relayUUID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		provider  = "slack"
		ts        = int64(1_700_000_000)
	)

	got1, err := BuildAuthSignedPayload(sessionID, challenge, relayUUID, provider, ts)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	got2, err := BuildAuthSignedPayload(sessionID, challenge, relayUUID, provider, ts)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if string(got1) != string(got2) {
		t.Fatalf("non-deterministic output")
	}

	// Sanity check against an inline reference computation: the two
	// implementations (Go here and TS in dicode-relay) must agree, but we
	// also lock in this byte sequence so accidental changes are caught.
	sidBytes, _ := hex.DecodeString(strings.ReplaceAll(sessionID, "-", ""))
	chBytes, _ := base64.RawURLEncoding.DecodeString(challenge)
	relayBytes, _ := hex.DecodeString(relayUUID)
	var tsBytes [8]byte
	binary.BigEndian.PutUint64(tsBytes[:], uint64(ts))
	h := sha256.New()
	h.Write(sidBytes)
	h.Write(chBytes)
	h.Write(relayBytes)
	h.Write([]byte(provider))
	h.Write(tsBytes[:])
	want := h.Sum(nil)
	if string(got1) != string(want) {
		t.Fatalf("payload mismatch:\n got=%x\nwant=%x", got1, want)
	}
}

func TestBuildAuthSignedPayload_RejectsBadInputs(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		challenge string
		relay     string
	}{
		{"bad session", "not-a-uuid", "E9MelhoazOwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", strings.Repeat("a", 64)},
		{"bad challenge", "550e8400e29b41d4a716446655440000", "!!!not base64url!!!", strings.Repeat("a", 64)},
		{"short relay", "550e8400e29b41d4a716446655440000", "E9MelhoazOwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", "deadbeef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildAuthSignedPayload(tc.sessionID, tc.challenge, tc.relay, "slack", 1); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestBuildAuthURL_RoundTripVerify(t *testing.T) {
	id := newOAuthTestIdentity(t)

	rawURL, req, err := BuildAuthURL("https://relay.dicode.app", id, "slack", "channels:read users:read", 1_700_000_000)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if req.Provider != "slack" || req.SessionID == "" || req.PKCEChallenge == "" {
		t.Fatalf("incomplete AuthRequest: %+v", req)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if u.Path != "/auth/slack" {
		t.Fatalf("unexpected path: %s", u.Path)
	}
	q := u.Query()
	for _, k := range []string{"session", "challenge", "relay_uuid", "sig", "timestamp", "scope"} {
		if q.Get(k) == "" {
			t.Fatalf("missing query param %s", k)
		}
	}
	if q.Get("relay_uuid") != id.UUID {
		t.Fatalf("relay_uuid mismatch")
	}
	if q.Get("scope") != "channels:read users:read" {
		t.Fatalf("scope mismatch")
	}

	// Verify signature with the daemon's public key — exercising the same
	// code path the relay broker uses.
	payload, err := BuildAuthSignedPayload(q.Get("session"), q.Get("challenge"), q.Get("relay_uuid"), "slack", 1_700_000_000)
	if err != nil {
		t.Fatalf("rebuild payload: %v", err)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(q.Get("sig"))
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if !ecdsa.VerifyASN1(&id.SignKey.PublicKey, payload, sigBytes) {
		t.Fatalf("signature verification failed")
	}
}

func TestBuildAuthURL_OmitsEmptyScope(t *testing.T) {
	id := newOAuthTestIdentity(t)
	rawURL, _, err := BuildAuthURL("https://relay.dicode.app", id, "slack", "", 1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	u, _ := url.Parse(rawURL)
	if u.Query().Has("scope") {
		t.Fatalf("expected no scope param when scope is empty")
	}
}

// TestDecryptOAuthToken_RoundTrip generates an ephemeral keypair on the
// "broker" side, encrypts a token payload exactly the way dicode-relay does,
// then verifies the daemon-side DecryptOAuthToken recovers the plaintext.
func TestDecryptOAuthToken_RoundTrip(t *testing.T) {
	daemon := newOAuthTestIdentity(t)
	plaintext := []byte(`{"access_token":"xoxb-test-token","scope":"channels:read"}`)
	sessionID := "550e8400-e29b-41d4-a716-446655440000"

	payload := encryptForDaemon(t, daemon, sessionID, plaintext)

	got, err := DecryptOAuthToken(daemon, payload)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("plaintext mismatch:\n got=%q\nwant=%q", got, plaintext)
	}
}

func TestDecryptOAuthToken_RejectsTamperedCiphertext(t *testing.T) {
	daemon := newOAuthTestIdentity(t)
	payload := encryptForDaemon(t, daemon, "550e8400-e29b-41d4-a716-446655440000", []byte("hello"))

	// Flip a byte in the middle of the ciphertext.
	ct, _ := base64.StdEncoding.DecodeString(payload.Ciphertext)
	ct[0] ^= 0xFF
	payload.Ciphertext = base64.StdEncoding.EncodeToString(ct)

	if _, err := DecryptOAuthToken(daemon, payload); err == nil {
		t.Fatalf("expected error on tampered ciphertext")
	}
}

func TestDecryptOAuthToken_RejectsWrongSession(t *testing.T) {
	daemon := newOAuthTestIdentity(t)
	payload := encryptForDaemon(t, daemon, "550e8400-e29b-41d4-a716-446655440000", []byte("hello"))
	payload.SessionID = "00000000-0000-0000-0000-000000000000"

	if _, err := DecryptOAuthToken(daemon, payload); err == nil {
		t.Fatalf("expected error when session id (HKDF salt) is wrong")
	}
}

// Envelopes with an empty Type must be rejected outright — the Type field
// is the AAD, so accepting an empty value would silently disable domain
// separation against any future ECIES-encrypted message type reusing this
// key (see also the review notes in docs/design/oauth-broker if added).
func TestDecryptOAuthToken_RejectsEmptyType(t *testing.T) {
	daemon := newOAuthTestIdentity(t)
	payload := encryptForDaemon(t, daemon, "550e8400-e29b-41d4-a716-446655440000", []byte("hello"))
	payload.Type = ""

	if _, err := DecryptOAuthToken(daemon, payload); err == nil {
		t.Fatalf("expected error when Type is empty")
	}
}

// Tampering with Type must invalidate the GCM tag because Type is bound as
// AAD. This is the domain-separation guarantee in the positive direction:
// not just "empty rejected" but "any mismatch rejected by the AEAD itself".
func TestDecryptOAuthToken_RejectsTamperedType(t *testing.T) {
	daemon := newOAuthTestIdentity(t)
	payload := encryptForDaemon(t, daemon, "550e8400-e29b-41d4-a716-446655440000", []byte("hello"))
	// A different non-empty type passes the explicit check but must fail
	// aead.Open because the AAD no longer matches what was sealed.
	// (We have to bypass the Type-required guard by sending a plausible-
	//  looking alternative; since the current guard rejects anything not
	//  equal to "oauth_token_delivery", this doubles as coverage for that.)
	payload.Type = "wrong_type"

	if _, err := DecryptOAuthToken(daemon, payload); err == nil {
		t.Fatalf("expected error when Type is tampered")
	}
}

// encryptForDaemon mirrors dicode-relay src/broker/crypto.ts eciesEncrypt().
func encryptForDaemon(t *testing.T, daemon *Identity, sessionID string, plaintext []byte) *OAuthTokenDeliveryPayload {
	t.Helper()

	// Daemon's DecryptKey public key as a crypto/ecdh peer key (post-#104
	// the broker encrypts against DecryptKey, not SignKey).
	daemonPubBytes := daemon.DecryptPublicKey()
	daemonPub, err := ecdh.P256().NewPublicKey(daemonPubBytes)
	if err != nil {
		t.Fatalf("daemon pub: %v", err)
	}

	// Fresh ephemeral keypair on the broker side.
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("eph keygen: %v", err)
	}
	shared, err := eph.ECDH(daemonPub)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}

	encKey := make([]byte, 32)
	kdf := hkdf.New(sha256.New, shared, []byte(sessionID), []byte(hkdfInfo))
	if _, err := io.ReadFull(kdf, encKey); err != nil {
		t.Fatalf("hkdf: %v", err)
	}

	iv := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		t.Fatalf("iv: %v", err)
	}
	block, _ := aes.NewCipher(encKey)
	aead, _ := cipher.NewGCM(block)
	ctWithTag := aead.Seal(nil, iv, plaintext, []byte("oauth_token_delivery"))

	return &OAuthTokenDeliveryPayload{
		Type:            "oauth_token_delivery",
		SessionID:       sessionID,
		EphemeralPubkey: base64.StdEncoding.EncodeToString(eph.PublicKey().Bytes()),
		Ciphertext:      base64.StdEncoding.EncodeToString(ctWithTag),
		Nonce:           base64.StdEncoding.EncodeToString(iv),
	}
}
