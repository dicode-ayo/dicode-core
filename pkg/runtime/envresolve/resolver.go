package envresolve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
)

// ProviderRequest is one entry the consumer needs from a provider.
type ProviderRequest struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional"`
}

// ProviderResult is the map a provider task returned via
// dicode.output(map, { secret: true }).
type ProviderResult struct {
	Values map[string]string
}

// ProviderRunner is the dependency through which the resolver invokes a
// provider task. The real implementation is the trigger engine, but the
// resolver tests inject a fake.
//
// The runner spawns the provider task with the request list JSON-encoded
// at params["requests"] (a bare JSON array of {name, optional} objects),
// and blocks until it returns. A non-nil error means the run did not
// complete successfully (timeout, crash, missing secret: true flag, etc.).
type ProviderRunner interface {
	Run(ctx context.Context, providerID string, reqs []ProviderRequest) (*ProviderResult, error)
}

// Registry is the subset of *registry.Registry the resolver needs. Tests
// pass a minimal fake.
type Registry interface {
	Get(id string) (*task.Spec, bool)
}

// Resolver resolves an env permissions block.
type Resolver struct {
	Runner   ProviderRunner
	Registry Registry
	Secrets  secrets.Chain
	Cache    *cache
	// Now defaults to time.Now if nil; tests inject a stable clock.
	Now func() time.Time
}

// New constructs a Resolver. Each runtime constructs its own Resolver;
// the cache lives inside the Resolver instance.
func New(reg Registry, sc secrets.Chain, runner ProviderRunner) *Resolver {
	return &Resolver{
		Runner:   runner,
		Registry: reg,
		Secrets:  sc,
		Cache:    newCache(),
		Now:      time.Now,
	}
}

// Resolved is the output of a resolution pass.
type Resolved struct {
	// Env is the variable name → value map to inject into the consumer
	// process environment.
	Env map[string]string
	// Secrets is the subset of Env whose values were sourced from a
	// secrets store or a provider task. Caller feeds this to the run-log
	// redactor (pkg/secrets.NewRedactor).
	Secrets map[string]string
}

// taskEntry is the per-entry info the provider-batch step needs.
type taskEntry struct {
	envName   string
	secretKey string
	optional  bool
}

// Resolve walks spec.Permissions.Env and produces a Resolved. Errors are
// returned typed (ErrProviderUnavailable / ErrRequiredSecretMissing /
// ErrProviderMisconfigured) so the trigger engine can categorize the
// failure for the run log.
//
// The consumer's RunStatus is the caller's responsibility: when Resolve
// errors, the caller marks the consumer run failed with the typed
// reason BEFORE spawning the consumer process.
func (r *Resolver) Resolve(ctx context.Context, spec *task.Spec) (*Resolved, error) {
	out := &Resolved{
		Env:     make(map[string]string, len(spec.Permissions.Env)),
		Secrets: make(map[string]string),
	}
	byProvider := make(map[string][]taskEntry)
	for _, e := range spec.Permissions.Env {
		if e.Value != "" {
			out.Env[e.Name] = e.Value
			continue
		}
		if e.Secret != "" {
			val, err := r.Secrets.Resolve(ctx, e.Secret)
			if err != nil {
				var notFound *secrets.NotFoundError
				if e.Optional && errors.As(err, &notFound) {
					out.Env[e.Name] = ""
					continue
				}
				return nil, fmt.Errorf("resolve secret %q for env %q: %w", e.Secret, e.Name, err)
			}
			out.Env[e.Name] = val
			out.Secrets[e.Name] = val
			continue
		}
		kind, target := task.ParseFrom(e.From)
		switch kind {
		case task.FromKindTask:
			byProvider[target] = append(byProvider[target], taskEntry{
				envName:   e.Name,
				secretKey: e.Name,
				optional:  e.Optional,
			})
		case task.FromKindEnv:
			if target != "" {
				out.Env[e.Name] = os.Getenv(target)
			}
			// fully bare → no injection (allowlist only); leave unset.
		}
	}
	providerIDs := make([]string, 0, len(byProvider))
	for id := range byProvider {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)
	for _, providerID := range providerIDs {
		if err := r.resolveProvider(ctx, providerID, byProvider[providerID], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// resolveProvider handles one provider's batch. It mutates the entries
// slice via sort.SliceStable; safe because callers (Resolve) always
// build entries fresh per call.
func (r *Resolver) resolveProvider(
	ctx context.Context,
	providerID string,
	entries []taskEntry,
	out *Resolved,
) error {
	// Single registry snapshot per Resolve call; cross-call registry
	// updates are handled by the cache's content-hash mismatch path.
	spec, ok := r.Registry.Get(providerID)
	if !ok {
		return &ErrProviderUnavailable{
			ProviderID: providerID,
			Cause:      fmt.Errorf("provider task not registered"),
		}
	}

	providerHash, _ := contentHashOf(spec)
	now := r.Now()

	// Sort entries so cache-miss request ordering is deterministic for tests.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].envName < entries[j].envName })

	misses := make([]ProviderRequest, 0, len(entries))
	cached := make(map[string]string)
	for _, e := range entries {
		if v, ok := r.Cache.get(providerID, e.secretKey, providerHash, now); ok {
			cached[e.envName] = v
			continue
		}
		misses = append(misses, ProviderRequest{Name: e.secretKey, Optional: e.optional})
	}

	var fetched map[string]string
	if len(misses) > 0 {
		res, err := r.Runner.Run(ctx, providerID, misses)
		if err != nil {
			return &ErrProviderUnavailable{ProviderID: providerID, Cause: err}
		}
		if res == nil || res.Values == nil {
			return &ErrProviderMisconfigured{
				ProviderID: providerID,
				Reason:     "task returned no secret map (did it call dicode.output(..., { secret: true })?)",
			}
		}
		fetched = res.Values

		ttl := time.Duration(0)
		if spec.Provider != nil {
			ttl = spec.Provider.CacheTTL
		}
		// Empty values are cached intentionally. A provider that returns
		// {"KEY": ""} is asserting the secret deliberately maps to empty;
		// we preserve that intent rather than treating empty as "not found".
		// Providers that mean "not found" should omit the key from the map
		// (the required/optional check handles that path below).
		for _, m := range misses {
			if v, present := fetched[m.Name]; present {
				r.Cache.put(providerID, m.Name, providerHash, v, ttl, now)
			}
		}
	}

	for _, e := range entries {
		val, fromCache := cached[e.envName]
		if !fromCache {
			v, present := fetched[e.secretKey]
			if !present {
				if e.optional {
					out.Env[e.envName] = ""
					continue
				}
				return &ErrRequiredSecretMissing{ProviderID: providerID, Key: e.secretKey}
			}
			val = v
		}
		out.Env[e.envName] = val
		out.Secrets[e.envName] = val
	}
	return nil
}

// contentHashOf returns the task content hash. Wraps task.Hash so the
// resolver can cache by it without depending on the loader directly.
//
// Returns ("", err) on read failure; callers treat empty hash as "always
// invalidate" via the cache's content-hash mismatch path.
func contentHashOf(spec *task.Spec) (string, error) {
	if spec.TaskDir == "" {
		return "", nil
	}
	h, err := task.Hash(spec.TaskDir)
	if err != nil {
		return "", err
	}
	return h, nil
}
