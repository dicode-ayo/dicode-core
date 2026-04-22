package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

// Tests for the ECDSA challenge-response used during the relay handshake
// (pkg/relay/client.go:signChallenge + the broker-side verify). Covers
// issue #124 item 2 — the daemon signs sha256(nonce || ts) with its SignKey;
// the broker ECDSA-verifies with the advertised public key.
//
// signChallenge is unexported, so these tests live in the relay package so
// they can exercise the exact wire contract. The "broker verify" path is
// reproduced with stdlib ecdsa.VerifyASN1 over the same digest — that is
// what the broker does in dicode-relay/src/broker.

// verifyChallenge is the broker-side verifier. Inline here so the tests
// stay in-package; mirrors exactly what the broker does on receipt of a
// hello message.
func verifyChallenge(pub *ecdsa.PublicKey, nonce []byte, ts int64, sig []byte) bool {
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(ts))
	h := sha256.New()
	h.Write(nonce)
	h.Write(tsBuf[:])
	return ecdsa.VerifyASN1(pub, h.Sum(nil), sig)
}

func mustNonce(t *testing.T) []byte {
	t.Helper()
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return nonce
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// TestSignChallenge_VerifiesWithCorrectKey is the positive path: a valid
// signature over sha256(nonce || ts) verifies with the corresponding
// public key.
func TestSignChallenge_VerifiesWithCorrectKey(t *testing.T) {
	key := mustKey(t)
	nonce := mustNonce(t)
	const ts int64 = 1_700_000_000

	sig, err := signChallenge(key, nonce, ts)
	if err != nil {
		t.Fatalf("signChallenge: %v", err)
	}
	if !verifyChallenge(&key.PublicKey, nonce, ts, sig) {
		t.Fatal("verify failed for a legitimate signature over (nonce, ts)")
	}
}

// TestSignChallenge_ForgedSigRejected covers the most direct attack: the
// broker must reject a random-bytes signature. Validates that ASN.1
// parsing + ECDSA verify genuinely guard the handshake.
func TestSignChallenge_ForgedSigRejected(t *testing.T) {
	key := mustKey(t)
	nonce := mustNonce(t)
	const ts int64 = 1_700_000_000

	// Random bytes in the shape of an ASN.1-DER ECDSA sig (doesn't matter —
	// VerifyASN1 must reject anything that isn't a valid signature).
	forged := make([]byte, 72)
	if _, err := rand.Read(forged); err != nil {
		t.Fatalf("rand: %v", err)
	}

	if verifyChallenge(&key.PublicKey, nonce, ts, forged) {
		t.Fatal("forged signature passed verify")
	}
}

// TestSignChallenge_TamperedSigRejected covers the subtler case: take a
// real signature and flip a byte. Catches regressions where a loose verify
// implementation might accept a malleable / partially-matching sig.
func TestSignChallenge_TamperedSigRejected(t *testing.T) {
	key := mustKey(t)
	nonce := mustNonce(t)
	const ts int64 = 1_700_000_000

	sig, err := signChallenge(key, nonce, ts)
	if err != nil {
		t.Fatalf("signChallenge: %v", err)
	}
	// Flip the last byte (inside the S value of the ASN.1 SEQUENCE).
	sig[len(sig)-1] ^= 0xFF

	if verifyChallenge(&key.PublicKey, nonce, ts, sig) {
		t.Fatal("tampered signature passed verify")
	}
}

// TestSignChallenge_WrongNonceRejected proves challenge binding. A
// signature legitimately issued for nonce A must NOT verify against
// nonce B — without this, a broker could replay a captured sig across
// different challenges (i.e. nonce reuse by a malicious peer).
func TestSignChallenge_WrongNonceRejected(t *testing.T) {
	key := mustKey(t)
	nonceA := mustNonce(t)
	nonceB := mustNonce(t)
	const ts int64 = 1_700_000_000

	sig, err := signChallenge(key, nonceA, ts)
	if err != nil {
		t.Fatalf("signChallenge: %v", err)
	}
	if verifyChallenge(&key.PublicKey, nonceB, ts, sig) {
		t.Fatal("signature over nonceA verified against nonceB — challenge binding broken")
	}
}

// TestSignChallenge_WrongTimestampRejected proves timestamp binding. Two
// signatures with different ts values over the same nonce must produce
// different valid sigs, and neither should verify against the other's ts.
// This is what prevents an attacker from replaying a stale sig at a later
// wall-clock check.
func TestSignChallenge_WrongTimestampRejected(t *testing.T) {
	key := mustKey(t)
	nonce := mustNonce(t)

	sig, err := signChallenge(key, nonce, 1_700_000_000)
	if err != nil {
		t.Fatalf("signChallenge: %v", err)
	}

	// Try to verify the exact same sig against a different ts.
	if verifyChallenge(&key.PublicKey, nonce, 1_700_000_001, sig) {
		t.Fatal("signature over ts=1_700_000_000 verified at ts=1_700_000_001 — timestamp binding broken")
	}
}

// TestSignChallenge_WrongPubkeyRejected proves key binding: a signature
// issued by identity A must not verify against identity B's public key.
func TestSignChallenge_WrongPubkeyRejected(t *testing.T) {
	keyA := mustKey(t)
	keyB := mustKey(t)
	nonce := mustNonce(t)
	const ts int64 = 1_700_000_000

	sig, err := signChallenge(keyA, nonce, ts)
	if err != nil {
		t.Fatalf("signChallenge: %v", err)
	}
	if verifyChallenge(&keyB.PublicKey, nonce, ts, sig) {
		t.Fatal("signature from key A verified against key B's public key")
	}
}

// TestSignChallenge_DigestIsNot_NonceAlone is a sanity check against a
// particular regression: if someone ever "simplifies" signChallenge to sign
// just the nonce (dropping ts), the timestamp guard in
// TestSignChallenge_WrongTimestampRejected might not fire for all digests
// (since both verify outcomes would be false). This test makes the
// digest shape explicit: verify the signature manually using a digest that
// omits ts, and confirm THAT does NOT pass — which means signChallenge is
// actually binding ts into the digest.
func TestSignChallenge_DigestIsNot_NonceAlone(t *testing.T) {
	key := mustKey(t)
	nonce := mustNonce(t)
	const ts int64 = 1_700_000_000

	sig, err := signChallenge(key, nonce, ts)
	if err != nil {
		t.Fatalf("signChallenge: %v", err)
	}

	// Verify with the *wrong* digest (nonce only). Should fail — if it
	// passes, signChallenge dropped ts from the hash.
	nonceOnly := sha256.Sum256(nonce)
	if ecdsa.VerifyASN1(&key.PublicKey, nonceOnly[:], sig) {
		t.Fatal("sig verified against sha256(nonce) alone — signChallenge failed to include ts")
	}
}
