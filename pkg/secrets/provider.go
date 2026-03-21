// Package secrets defines the SecretProvider interface and the resolution
// chain used to inject secrets into task runtimes.
//
// Tasks declare which env vars they need in task.yaml under `env:`.
// At runtime, each var is resolved by walking the provider chain in order —
// first provider that returns a non-empty value wins.
//
// MVP providers: local (encrypted SQLite) and env (host environment).
// Additional providers (Vault, AWS SM, etc.) implement the same interface.
package secrets

import (
	"context"
	"fmt"
)

// Provider resolves a secret by name.
type Provider interface {
	// Name returns a human-readable identifier used in logs and error messages.
	Name() string

	// Get returns the secret value for key, or ("", nil) if not found.
	// A non-nil error means the provider experienced a failure (network down,
	// auth error, etc.) — distinct from a key simply not existing.
	Get(ctx context.Context, key string) (string, error)
}

// Chain resolves secrets by trying each provider in order.
// First non-empty result wins.
type Chain []Provider

// Resolve walks the chain and returns the first non-empty value found.
// Returns ErrNotFound if no provider has the key.
func (c Chain) Resolve(ctx context.Context, key string) (string, error) {
	for _, p := range c {
		val, err := p.Get(ctx, key)
		if err != nil {
			return "", fmt.Errorf("provider %s: %w", p.Name(), err)
		}
		if val != "" {
			return val, nil
		}
	}
	return "", &NotFoundError{Key: key}
}

// ResolveAll resolves a list of keys and returns a map.
// Returns the first error encountered.
func (c Chain) ResolveAll(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		val, err := c.Resolve(ctx, key)
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
	return out, nil
}

// NotFoundError is returned when no provider in the chain has the requested key.
type NotFoundError struct {
	Key string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("secret %q not found in any configured provider", e.Key)
}
