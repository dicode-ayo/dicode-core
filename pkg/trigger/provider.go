// This file contains the Engine's provider-running surface: the
// envresolve.ProviderRunner implementation (Run), the runtime wiring for
// secret-output channels, the shared env resolver accessor, and the
// pre-dispatch env-resolution preflight.

package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// DenoRuntimeAPI is the minimal subset of *deno.Runtime the engine's
// ProviderRunner implementation depends on. Defined here (not imported)
// to keep pkg/trigger free of pkg/runtime/deno; daemon.go wires the real
// runtime via SetDenoRuntime.
type DenoRuntimeAPI interface {
	SetSecretOutputChannel(ch chan map[string]string)
}

// PythonRuntimeAPI is the minimal subset of *python.Runtime the engine's
// ProviderRunner implementation depends on. Mirrors DenoRuntimeAPI.
type PythonRuntimeAPI interface {
	SetSecretOutputChannel(ch chan map[string]string)
}

// SetDenoRuntime wires the deno runtime so the engine can act as a
// ProviderRunner — swapping the per-run SecretOutputChannel before
// firing a provider task and clearing it after.
func (e *Engine) SetDenoRuntime(r DenoRuntimeAPI) { e.denoRuntime = r }

// SetPythonRuntime wires the python runtime; mirror of SetDenoRuntime.
func (e *Engine) SetPythonRuntime(r PythonRuntimeAPI) { e.pythonRuntime = r }

// Resolver returns the daemon-scoped env resolver, constructing it lazily on
// first call. The resolver's TTL cache survives across task launches so that
// provider.cache_ttl actually provides cross-fire benefit (issue #242).
func (e *Engine) Resolver() *envresolve.Resolver {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.envResolver == nil {
		if e.secrets == nil {
			e.log.Warn("Resolver() called before SetSecrets() — resolver will have no secrets chain")
		}
		e.envResolver = envresolve.New(e.registry, e.secrets, e)
	}
	return e.envResolver
}

// Run satisfies envresolve.ProviderRunner. It spawns the provider task
// synchronously and waits for it to finish; the secret map is collected
// over the IPC channel pre-wired into the runtime by SetSecretOutputChannel.
//
// Concurrency: serialized through providerRunMu because the runtime's
// secretOutputCh is single-slot global state. MVP-quality — see
// providerRunMu doc on the Engine struct.
//
// Errors:
//   - ctx.Err() if the caller context expires
//   - error if the spawn fails or the run errors out
//   - error if the run finished without sending a map (provider didn't
//     call output(..., {secret: true}))
func (e *Engine) Run(ctx context.Context, providerID string, reqs []envresolve.ProviderRequest) (*envresolve.ProviderResult, error) {
	spec, ok := e.registry.Get(providerID)
	if !ok {
		return nil, fmt.Errorf("provider task %q not registered", providerID)
	}

	e.providerRunMu.Lock()
	defer e.providerRunMu.Unlock()

	ch := make(chan map[string]string, 1)
	switch spec.Runtime {
	case task.RuntimeDeno, "", "js":
		if e.denoRuntime == nil {
			return nil, fmt.Errorf("deno runtime not wired to engine")
		}
		e.denoRuntime.SetSecretOutputChannel(ch)
		defer e.denoRuntime.SetSecretOutputChannel(nil)
	default:
		if e.pythonRuntime == nil {
			return nil, fmt.Errorf("python runtime not wired to engine (runtime=%q)", spec.Runtime)
		}
		e.pythonRuntime.SetSecretOutputChannel(ch)
		defer e.pythonRuntime.SetSecretOutputChannel(nil)
	}

	reqJSON, _ := json.Marshal(reqs)
	runID, err := e.fireAsync(ctx, spec, pkgruntime.RunOptions{
		Params: map[string]string{"requests": string(reqJSON)},
	}, "provider")
	if err != nil {
		return nil, fmt.Errorf("fire provider %q: %w", providerID, err)
	}
	res, werr := e.waitRunSettled(ctx, runID)
	if werr != nil {
		return nil, fmt.Errorf("wait provider %q: %w", providerID, werr)
	}
	if res.Status != registry.StatusSuccess {
		return nil, fmt.Errorf("provider %q run %s: %s", providerID, runID, res.Status)
	}

	// The buffered (cap=1) channel was populated when the IPC server
	// observed the dicode.output(..., {secret:true}) call — by the time
	// WaitRun returns success the value is already enqueued. A short
	// non-blocking read with a tiny safety timeout diagnoses providers
	// that completed without ever calling output(secret).
	select {
	case sm := <-ch:
		return &envresolve.ProviderResult{Values: sm}, nil
	case <-time.After(50 * time.Millisecond):
		return nil, fmt.Errorf("provider %q completed without secret output (did it call dicode.output(map, { secret: true })?)", providerID)
	}
}

// preflightEnv runs the env resolver once before dispatch so that typed
// provider failures (provider_unavailable / required_secret_missing /
// provider_misconfigured) can be recorded as the run's fail_reason
// instead of surfacing as opaque dispatch errors.
//
// On success, it returns the *Resolved so dispatch can hand it to the
// runtime via RunOptions.PreResolvedEnv, ensuring provider tasks fire
// exactly once per consumer launch instead of twice (issue #235).
//
// Return contract:
//   - success: (resolved, "", "")
//   - typed envresolve failure: (nil, registry.StatusFailure, "<reason>")
//   - non-typed error or skipped (no secrets chain / no env entries):
//     (nil, "", "") — dispatch proceeds and the runtime resolves inline.
func (e *Engine) preflightEnv(ctx context.Context, spec *task.Spec) (*envresolve.Resolved, string, string) {
	// Skip preflight when secrets chain isn't wired (test fixtures) or
	// when the spec has no env entries the resolver could fail on.
	if e.secrets == nil || len(spec.Permissions.Env) == 0 {
		return nil, "", ""
	}
	resolved, err := e.Resolver().Resolve(ctx, spec)
	if err != nil {
		var pu *envresolve.ErrProviderUnavailable
		var rsm *envresolve.ErrRequiredSecretMissing
		var mis *envresolve.ErrProviderMisconfigured
		switch {
		case errors.As(err, &pu):
			return nil, registry.StatusFailure, "provider_unavailable: " + pu.ProviderID
		case errors.As(err, &rsm):
			return nil, registry.StatusFailure, "required_secret_missing: " + rsm.Key + " from " + rsm.ProviderID
		case errors.As(err, &mis):
			return nil, registry.StatusFailure, "provider_misconfigured: " + mis.ProviderID
		}
		// Non-typed error: log for operator visibility (without the error
		// detail, which may contain secret key names), then let dispatch
		// surface it through the runtime's inline resolver path.
		e.log.Warn("preflight env-resolve returned non-typed error — falling through to inline resolution",
			zap.String("task", spec.ID))
		return nil, "", ""
	}
	return resolved, "", ""
}
