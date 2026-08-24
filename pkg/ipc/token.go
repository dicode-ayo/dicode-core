package ipc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	tokenTTL    = 24 * time.Hour
	tokenCLITTL = 10 * 365 * 24 * time.Hour // CLI tokens are valid for ~10 years; daemon restart re-issues anyway
)

// tokenClaims is the payload embedded in a capability token.
type tokenClaims struct {
	Identity string   `json:"id"`  // e.g. "task:my-task-id"
	RunID    string   `json:"run"` // run correlation id
	Caps     []string `json:"caps"`
	Exp      int64    `json:"exp"` // Unix timestamp
}

// IssueToken creates a signed capability token for the given identity and caps.
// secret is a per-daemon random secret; runID ties the token to a specific run.
func IssueToken(secret []byte, identity, runID string, caps []string) (string, error) {
	return issueToken(secret, identity, runID, caps, tokenTTL)
}

// IssueTokenWithTTL is like IssueToken but with an explicit TTL.
func IssueTokenWithTTL(secret []byte, identity, runID string, caps []string, ttl time.Duration) (string, error) {
	return issueToken(secret, identity, runID, caps, ttl)
}

func issueToken(secret []byte, identity, runID string, caps []string, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", errEmptySecret
	}
	claims := tokenClaims{
		Identity: identity,
		RunID:    runID,
		Caps:     caps,
		Exp:      time.Now().Add(ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString(tokenSig(secret, encoded))
	return encoded + "." + sig, nil
}

// errEmptySecret guards IssueToken/VerifyToken against a nil or zero-length
// signing key. hmac.New places no restriction on key length — a nil key
// degrades to an all-zero key, not to "unauthenticated" — so without this
// check a caller that forgot to wire a real secret (as the per-version Deno
// executor did before issue #718) would silently mint and accept tokens
// signed under a fixed, publicly-known key instead of failing loudly. Fail
// closed instead: refuse to issue or verify under an empty key.
var errEmptySecret = errors.New("ipc: secret must not be empty")

// VerifyToken validates the token signature and expiry, returning the claims.
func VerifyToken(secret []byte, tok string) (tokenClaims, error) {
	if len(secret) == 0 {
		return tokenClaims{}, errEmptySecret
	}
	dot := strings.LastIndex(tok, ".")
	if dot < 0 {
		return tokenClaims{}, errors.New("ipc: malformed token")
	}
	encoded, sigStr := tok[:dot], tok[dot+1:]
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil || !hmac.Equal(tokenSig(secret, encoded), sigBytes) {
		return tokenClaims{}, errors.New("ipc: invalid token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return tokenClaims{}, fmt.Errorf("ipc: token decode: %w", err)
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return tokenClaims{}, fmt.Errorf("ipc: token unmarshal: %w", err)
	}
	if time.Now().Unix() > claims.Exp {
		return tokenClaims{}, errors.New("ipc: token expired")
	}
	return claims, nil
}

func tokenSig(secret []byte, encoded string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encoded))
	return mac.Sum(nil)
}

// hasCap reports whether caps contains the given capability.
func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// NewSecret generates a random 32-byte daemon secret.
func NewSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
