package envresolve

import "fmt"

// ErrProviderUnavailable is returned when a provider task spawn / IPC /
// timeout / non-zero exit prevents the resolver from collecting the map.
// The trigger engine renders this as a typed failure reason
// "provider_unavailable: <providerID>".
type ErrProviderUnavailable struct {
	ProviderID string
	Cause      error
}

func (e *ErrProviderUnavailable) Error() string {
	return fmt.Sprintf("provider %q unavailable: %v", e.ProviderID, e.Cause)
}

func (e *ErrProviderUnavailable) Unwrap() error { return e.Cause }

// ErrRequiredSecretMissing is returned when a provider task returned its
// secret map but a non-optional key the consumer requested is absent.
// The trigger engine renders this as a typed failure reason
// "required_secret_missing: <Key> from <ProviderID>".
type ErrRequiredSecretMissing struct {
	ProviderID string
	Key        string
}

func (e *ErrRequiredSecretMissing) Error() string {
	return fmt.Sprintf("required secret %q missing from provider %q", e.Key, e.ProviderID)
}

// ErrProviderMisconfigured is returned when a task referenced via
// from: task:<id> exists but did not call dicode.output(map, { secret:
// true }) — i.e. it is not actually a provider. Surfaced as a startup
// validation hint to the operator.
type ErrProviderMisconfigured struct {
	ProviderID string
	Reason     string
}

func (e *ErrProviderMisconfigured) Error() string {
	return fmt.Sprintf("provider %q misconfigured: %s", e.ProviderID, e.Reason)
}
