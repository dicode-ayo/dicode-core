package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/secrets"
)

// oauthSecretSuffixes is the fixed set of suffixes storeOAuthToken writes.
// Before writing a new bundle, all existing keys with these suffixes are
// deleted so a re-auth that omits a field (e.g. no refresh_token on the
// second Google consent) does not leave stale values from the prior auth.
var oauthSecretSuffixes = []string{
	"_ACCESS_TOKEN",
	"_REFRESH_TOKEN",
	"_EXPIRES_AT",
	"_SCOPE",
	"_TOKEN_TYPE",
}

// maxExpiresInSeconds caps the expires_in field to avoid int64 overflow when
// converting seconds to time.Duration (nanoseconds). 10 years is generous;
// no legitimate OAuth provider issues tokens that live longer.
const maxExpiresInSeconds = 10 * 365 * 24 * 3600

// storeOAuthToken parses a decrypted token bundle from the relay broker and
// writes its fields into the secrets store under a provider-scoped naming
// convention. Returns the list of secret names written so the caller can
// report them without ever touching the plaintext values.
//
// Before writing, the full set of <PREFIX>_* keys is deleted unconditionally.
// This prevents stale credentials from a prior auth surviving alongside
// fresh values from a new auth (e.g. old refresh_token alongside new
// access_token after a re-consent flow that didn't return a refresh_token).
//
// Naming: <PROVIDER>_ACCESS_TOKEN plus optional _REFRESH_TOKEN, _EXPIRES_AT,
// _SCOPE, _TOKEN_TYPE suffixes. Provider is upper-cased and sanitised so a
// malicious provider value cannot inject unexpected key prefixes.
func storeOAuthToken(ctx context.Context, mgr secrets.Manager, provider string, plaintext []byte) ([]string, error) {
	prefix, err := sanitizeProviderPrefix(provider)
	if err != nil {
		return nil, err
	}

	var raw map[string]any
	if err := json.Unmarshal(plaintext, &raw); err != nil {
		return nil, fmt.Errorf("parse token json: %w", err)
	}

	accessToken, _ := raw["access_token"].(string)
	if accessToken == "" {
		return nil, fmt.Errorf("token payload missing access_token")
	}

	// Delete all prior values under this prefix before writing the new set.
	// Errors on delete are tolerated (key may not exist).
	for _, suffix := range oauthSecretSuffixes {
		_ = mgr.Delete(ctx, prefix+suffix)
	}

	written := make([]string, 0, 5)
	setIf := func(name, value string) error {
		if value == "" {
			return nil
		}
		if err := mgr.Set(ctx, name, value); err != nil {
			return fmt.Errorf("set %s: %w", name, err)
		}
		written = append(written, name)
		return nil
	}

	if err := setIf(prefix+"_ACCESS_TOKEN", accessToken); err != nil {
		return nil, err
	}
	if refresh, _ := raw["refresh_token"].(string); refresh != "" {
		if err := setIf(prefix+"_REFRESH_TOKEN", refresh); err != nil {
			return nil, err
		}
	}
	if scope, _ := raw["scope"].(string); scope != "" {
		if err := setIf(prefix+"_SCOPE", scope); err != nil {
			return nil, err
		}
	}
	if tokenType, _ := raw["token_type"].(string); tokenType != "" {
		if err := setIf(prefix+"_TOKEN_TYPE", tokenType); err != nil {
			return nil, err
		}
	}

	// expires_in arrives as a JSON number; convert to an absolute RFC3339
	// timestamp so refresh tooling can compare without re-interpreting units.
	// Clamped to maxExpiresInSeconds to avoid int64 overflow in the
	// seconds → Duration(nanoseconds) conversion.
	var expiresIn int64
	switch v := raw["expires_in"].(type) {
	case float64:
		expiresIn = int64(v)
	case string:
		expiresIn, _ = strconv.ParseInt(v, 10, 64)
	}
	if expiresIn > maxExpiresInSeconds {
		expiresIn = maxExpiresInSeconds
	}
	if expiresIn > 0 {
		expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339)
		if err := setIf(prefix+"_EXPIRES_AT", expiresAt); err != nil {
			return nil, err
		}
	}
	return written, nil
}

// sanitizeProviderPrefix upper-cases and restricts provider to [A-Z0-9_],
// rejecting anything else so an attacker who somehow plants a pending
// session with a crafted provider string cannot write to arbitrary secret
// keys (e.g. overwriting an existing secret via "slack_access_token;rm").
//
// Additional guards: minimum 2 characters and no leading/trailing underscore,
// to prevent collision with unrelated secret namespaces (e.g. a provider of
// "_FOO" would produce "__FOO_ACCESS_TOKEN").
func sanitizeProviderPrefix(provider string) (string, error) {
	p := strings.ToUpper(strings.TrimSpace(provider))
	if len(p) < 2 {
		return "", fmt.Errorf("provider must be at least 2 characters, got %q", provider)
	}
	if p[0] == '_' || p[len(p)-1] == '_' {
		return "", fmt.Errorf("provider must not start or end with underscore: %q", provider)
	}
	for _, r := range p {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return "", fmt.Errorf("invalid provider %q", provider)
		}
	}
	return p, nil
}
