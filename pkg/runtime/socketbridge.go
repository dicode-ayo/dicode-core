// This file holds the shared building blocks of the "socket-bridge" runtimes
// (Deno, Python): subprocess runtimes that talk to the daemon over a per-run
// Unix socket (pkg/ipc). The logic here used to be copy-pasted between
// pkg/runtime/deno and pkg/runtime/python (issue #388); it lives in this
// package alongside SubprocessEnv, following the same precedent.
package runtime

import (
	"context"
	"time"

	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
)

// MergeParams merges trigger-time param overrides over the spec's declared
// defaults. Declared params with an empty default are omitted; overrides win
// on duplicate names and may introduce names not declared in the spec.
func MergeParams(specParams []task.Param, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(specParams))
	for _, p := range specParams {
		if p.Default != "" {
			out[p.Name] = p.Default
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ResolveRunEnv resolves a run's declared env permissions, returning the
// name→value map to inject into the subprocess environment and a redactor
// over the secret values for the run-log streams.
//
// When the trigger engine ran preflight (issue #235) it forwards its
// *Resolved via pre so provider tasks aren't re-spawned. When pre is nil
// (legacy callers, tests that bypass the engine), resolver() is invoked for
// inline resolution — lazily, so no resolver is constructed on the preflight
// path. Provider tasks (from: task:<id>) are spawned and batched at most
// once per provider per launch; legacy paths (secret:, env:NAME, bare) are
// preserved.
func ResolveRunEnv(ctx context.Context, spec *task.Spec, pre *envresolve.Resolved, resolver func() *envresolve.Resolver) (map[string]string, *secrets.Redactor, error) {
	res := pre
	if res == nil {
		var err error
		res, err = resolver().Resolve(ctx, spec)
		if err != nil {
			return nil, nil, err
		}
	}
	return res.Env, secrets.NewRedactor(res.Secrets), nil
}

// LiveResolver picks the env resolver for a run. Precedence: the parent
// (manager) runtime's daemon-scoped shared resolver — SetEnvResolver may be
// called after per-version executors already exist, hence the live read
// through parent; then self's own shared resolver (the manager-owned
// instance, parent == nil); finally a fresh instance built from self's
// snapshot (legacy / test path). The shared resolver's TTL cache is what
// lets provider.cache_ttl survive across task launches (issue #242).
func LiveResolver(self, parent *BridgeDeps) *envresolve.Resolver {
	if parent != nil && parent.SharedResolver != nil {
		return parent.SharedResolver
	}
	if self.SharedResolver != nil {
		return self.SharedResolver
	}
	return envresolve.New(self.Registry, self.SecretsChain, self.ProviderRunner)
}

// ExecContext returns the context and cancel function for a task execution.
// A positive timeout creates a child context with that deadline. A zero or
// negative timeout wraps the parent in WithCancel so the caller can still
// cancel early but no new deadline is imposed (issue #389 unified this
// semantics across the Deno and Python runtimes).
func ExecContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}
