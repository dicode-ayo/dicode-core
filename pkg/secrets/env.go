package secrets

import (
	"context"
	"os"
)

// EnvProvider resolves secrets from host environment variables.
// Typically used as the last fallback in the provider chain.
type EnvProvider struct{}

func NewEnvProvider() *EnvProvider { return &EnvProvider{} }

func (e *EnvProvider) Name() string { return "env" }

func (e *EnvProvider) Get(_ context.Context, key string) (string, error) {
	return os.Getenv(key), nil
}
