package ipc

import (
	"context"
	"errors"
	"fmt"

	"github.com/dicode/dicode/pkg/secrets"
)

// maxStatusBatchSize bounds the per-call work of listOAuthStatus. Each entry
// performs up to four secret-store reads, so a hostile caller without this
// cap could amplify into an arbitrary number of provider lookups. The limit
// is generous enough to cover any realistic dashboard.
const maxStatusBatchSize = 64

// ProviderStatus is the per-provider response shape for dicode.oauth.list_status.
// HasToken is the only field derived from <P>_ACCESS_TOKEN — its plaintext
// is never surfaced to callers.
type ProviderStatus struct {
	Provider  string  `json:"provider"` // lowercase, as supplied by the caller
	HasToken  bool    `json:"has_token"`
	ExpiresAt *string `json:"expires_at,omitempty"` // RFC3339 string or absent
	Scope     *string `json:"scope,omitempty"`
	TokenType *string `json:"token_type,omitempty"`
}

// listOAuthStatus reads OAuth status metadata for each provider key the caller
// supplies. Plaintext access/refresh tokens are never read into the response;
// only the presence flag and the three metadata strings (expiry, scope,
// token type) are surfaced.
//
// Each provider name passes through sanitizeProviderPrefix (shared with
// storeOAuthToken) so a malicious caller cannot escape into arbitrary
// secret-key namespaces.
func listOAuthStatus(ctx context.Context, chain secrets.Chain, providers []string) ([]ProviderStatus, error) {
	if len(providers) > maxStatusBatchSize {
		return nil, fmt.Errorf("too many providers: %d > %d", len(providers), maxStatusBatchSize)
	}
	out := make([]ProviderStatus, 0, len(providers))
	for _, p := range providers {
		prefix, err := sanitizeProviderPrefix(p)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p, err)
		}
		access := resolveOrEmpty(ctx, chain, prefix+"_ACCESS_TOKEN")
		entry := ProviderStatus{
			Provider: p,
			HasToken: access != "",
		}
		if v := resolveOrEmpty(ctx, chain, prefix+"_EXPIRES_AT"); v != "" {
			entry.ExpiresAt = &v
		}
		if v := resolveOrEmpty(ctx, chain, prefix+"_SCOPE"); v != "" {
			entry.Scope = &v
		}
		if v := resolveOrEmpty(ctx, chain, prefix+"_TOKEN_TYPE"); v != "" {
			entry.TokenType = &v
		}
		out = append(out, entry)
	}
	return out, nil
}

// resolveOrEmpty wraps Chain.Resolve so a NotFoundError becomes empty string.
// Provider-error cases (network down, etc.) are also tolerated as empty for
// status-reporting purposes — the caller only needs presence/absence, and a
// transient backend hiccup should not fail the whole dashboard.
func resolveOrEmpty(ctx context.Context, chain secrets.Chain, key string) string {
	if chain == nil {
		return ""
	}
	v, err := chain.Resolve(ctx, key)
	if err != nil {
		var notFound *secrets.NotFoundError
		if errors.As(err, &notFound) {
			return ""
		}
		return ""
	}
	return v
}
