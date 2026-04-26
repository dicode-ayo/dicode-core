package ipc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/secrets"
)

func TestListOAuthStatus_EmptyInput(t *testing.T) {
	chain := chainFromMem(newMemSecrets())

	out, err := listOAuthStatus(context.Background(), chain, []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty result, got %d entries", len(out))
	}
}

func TestListOAuthStatus_FullBundle(t *testing.T) {
	ms := newMemSecrets()
	_ = ms.Set(context.Background(), "GITHUB_ACCESS_TOKEN", "ghp_xxx")
	_ = ms.Set(context.Background(), "GITHUB_EXPIRES_AT", "2026-12-31T00:00:00Z")
	_ = ms.Set(context.Background(), "GITHUB_SCOPE", "user repo")
	_ = ms.Set(context.Background(), "GITHUB_TOKEN_TYPE", "Bearer")
	chain := chainFromMem(ms)

	out, err := listOAuthStatus(context.Background(), chain, []string{"github"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	got := out[0]
	if got.Provider != "github" || !got.HasToken {
		t.Fatalf("provider/has_token wrong: %+v", got)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != "2026-12-31T00:00:00Z" {
		t.Fatalf("expires_at wrong: %v", got.ExpiresAt)
	}
	if got.Scope == nil || *got.Scope != "user repo" {
		t.Fatalf("scope wrong: %v", got.Scope)
	}
	if got.TokenType == nil || *got.TokenType != "Bearer" {
		t.Fatalf("token_type wrong: %v", got.TokenType)
	}
}

func TestListOAuthStatus_AccessTokenOnly(t *testing.T) {
	ms := newMemSecrets()
	_ = ms.Set(context.Background(), "OPENROUTER_ACCESS_TOKEN", "sk-or-xxx")
	chain := chainFromMem(ms)

	out, err := listOAuthStatus(context.Background(), chain, []string{"openrouter"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out[0]
	if !got.HasToken {
		t.Fatalf("expected has_token=true")
	}
	if got.ExpiresAt != nil || got.Scope != nil || got.TokenType != nil {
		t.Fatalf("expected metadata pointers nil; got %+v", got)
	}
}

func TestListOAuthStatus_NoTokenAtAll(t *testing.T) {
	chain := chainFromMem(newMemSecrets())

	out, err := listOAuthStatus(context.Background(), chain, []string{"slack"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out[0]
	if got.HasToken {
		t.Fatalf("expected has_token=false")
	}
}

func TestListOAuthStatus_MalformedName(t *testing.T) {
	chain := chainFromMem(newMemSecrets())

	for _, bad := range []string{"a", "_x", "x_", "X;Y", ""} {
		_, err := listOAuthStatus(context.Background(), chain, []string{bad})
		if err == nil {
			t.Fatalf("expected error for malformed %q, got nil", bad)
		}
	}
}

func TestListOAuthStatus_OversizeBatch(t *testing.T) {
	chain := chainFromMem(newMemSecrets())

	big := make([]string, maxStatusBatchSize+1)
	for i := range big {
		big[i] = "github"
	}
	_, err := listOAuthStatus(context.Background(), chain, big)
	if err == nil {
		t.Fatalf("expected oversize error, got nil")
	}
}

// TestListOAuthStatus_PlaintextNonLeakage confirms the access-token plaintext
// is read only to set HasToken and never appears in the marshalled response.
func TestListOAuthStatus_PlaintextNonLeakage(t *testing.T) {
	const sentinel = "SENTINEL_PLAINTEXT_TOKEN_aaaaaaaa"
	ms := newMemSecrets()
	_ = ms.Set(context.Background(), "FOO_ACCESS_TOKEN", sentinel)
	chain := chainFromMem(ms)

	out, err := listOAuthStatus(context.Background(), chain, []string{"foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), sentinel) {
		t.Fatalf("sentinel leaked into response: %s", body)
	}
	if !out[0].HasToken {
		t.Fatalf("expected has_token=true")
	}
}

// chainFromMem wraps memSecrets as a single-element secrets.Chain.
func chainFromMem(ms *memSecrets) secrets.Chain {
	return secrets.Chain{memProvider{ms}}
}

// memProvider adapts memSecrets to secrets.Provider (Get + Name).
type memProvider struct{ ms *memSecrets }

func (m memProvider) Name() string { return "memProvider" }
func (m memProvider) Get(ctx context.Context, key string) (string, error) {
	return m.ms.Get(ctx, key)
}
