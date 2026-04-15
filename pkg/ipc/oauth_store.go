package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/secrets"
)

// storeOAuthToken parses a decrypted token bundle from the relay broker and
// writes its fields into the secrets store under a provider-scoped naming
// convention. Returns the list of secret names written so the caller can
// report them without ever touching the plaintext values.
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
	var expiresIn int64
	switch v := raw["expires_in"].(type) {
	case float64:
		expiresIn = int64(v)
	case string:
		_, _ = fmt.Sscanf(v, "%d", &expiresIn)
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
func sanitizeProviderPrefix(provider string) (string, error) {
	p := strings.ToUpper(strings.TrimSpace(provider))
	if p == "" {
		return "", fmt.Errorf("provider required")
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
