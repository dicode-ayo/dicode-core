package ipc

import "testing"

// TestIssueTokenRejectsEmptySecret guards the fail-closed fix from issue
// #718: hmac.New places no restriction on key length, so a nil or
// zero-length secret does not degrade to "unauthenticated" — it degrades to
// signing under a fixed, publicly-known all-zero key. IssueToken must refuse
// to mint a token under such a key instead of silently succeeding.
func TestIssueTokenRejectsEmptySecret(t *testing.T) {
	for name, secret := range map[string][]byte{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := IssueToken(secret, "task:x", "run-1", []string{CapLog}); err == nil {
				t.Fatal("IssueToken succeeded with an empty secret; want an error")
			}
		})
	}
}

// TestVerifyTokenRejectsEmptySecret is the handshake-side half of the same
// guard: even if a token somehow got minted, verifying it under an empty
// secret must fail closed rather than accept it.
func TestVerifyTokenRejectsEmptySecret(t *testing.T) {
	real, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	tok, err := IssueToken(real, "task:x", "run-1", []string{CapLog})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	for name, secret := range map[string][]byte{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyToken(secret, tok); err == nil {
				t.Fatal("VerifyToken succeeded with an empty secret; want an error")
			}
		})
	}
}

// TestIssueTokenRoundTripsWithRealSecret is a sanity check that the empty
// guard above doesn't over-fire on the normal path.
func TestIssueTokenRoundTripsWithRealSecret(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	tok, err := IssueToken(secret, "task:x", "run-1", []string{CapLog, CapKVRead})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	claims, err := VerifyToken(secret, tok)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if claims.Identity != "task:x" || claims.RunID != "run-1" {
		t.Errorf("unexpected claims: %+v", claims)
	}
}
