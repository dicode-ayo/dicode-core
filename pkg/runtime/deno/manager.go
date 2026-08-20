package deno

import (
	"context"
	"os"

	denopkg "github.com/dicode/dicode/pkg/deno"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

// ManagedRuntime interface — lets the dicode runtime manager download, verify,
// and upgrade the Deno binary without knowing Deno-specific internals.

func (rt *Runtime) Name() string        { return "deno" }
func (rt *Runtime) DisplayName() string { return "Deno" }
func (rt *Runtime) Description() string {
	return "TypeScript/JavaScript runtime with npm support and the dicode SDK (log, kv, params, env, input, output)."
}
func (rt *Runtime) DefaultVersion() string { return denopkg.DefaultVersion }

// BinaryPath returns the expected cache path for the given Deno version.
func (rt *Runtime) BinaryPath(version string) (string, error) {
	return denopkg.BinaryPath(version)
}

// IsInstalled checks whether the Deno binary for the given version is cached.
func (rt *Runtime) IsInstalled(version string) bool {
	p, err := denopkg.BinaryPath(version)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Install downloads and caches the Deno binary for the given version.
func (rt *Runtime) Install(_ context.Context, version string) error {
	_, err := denopkg.EnsureDeno(version)
	return err
}

// NewExecutor returns a new Deno Executor that uses the binary at binaryPath.
// The new executor shares the registry, secrets, db, and logger with this
// Runtime — and propagates the issue #119 provider channels so the
// trigger-engine dispatch path actually sees the wired runner / sink.
//
// The executor holds a parent back-reference so that a late SetInputStore call
// on the manager (which happens in daemon.go after buildRuntimes returns) is
// visible to all executors via effectiveInputStore().
//
// The copy list matches the Python runtime's NewExecutor field-for-field
// (issue #718): both snapshot every construction-time BridgeDeps field —
// Registry, SecretsChain, SecretsManager, DB, Log, IPCSecret, Engine,
// Gateway, SecretOutputCh, ProviderRunner. Before #718 this list omitted
// SecretsManager, IPCSecret, Engine, and Gateway, so a per-version Deno
// executor (runtimes.deno.version pinned to an installed version) served
// every run with those four fields nil: dicode.run_task failed with
// "engine not available", dicode.http/secrets_set/secrets_delete were inert,
// and — the security-relevant one — per-run IPC capability tokens were
// minted and verified under a nil (empty) HMAC key instead of the daemon's
// real IPCSecret. daemon.go always calls SetEngine/SetGateway/
// SetSecretsManager on the manager before it calls NewExecutor for a pinned
// version, and IPCSecret is set at construction in New(), so copying these
// four here is sufficient — no field is genuinely unavailable at the time a
// per-version executor is built.
func (rt *Runtime) NewExecutor(binaryPath string) pkgruntime.Executor {
	return &Runtime{
		parent: rt,
		BridgeDeps: pkgruntime.BridgeDeps{
			Registry:       rt.Registry,
			SecretsChain:   rt.SecretsChain,
			SecretsManager: rt.SecretsManager,
			DB:             rt.DB,
			Log:            rt.Log,
			IPCSecret:      rt.IPCSecret,
			Engine:         rt.Engine,
			Gateway:        rt.Gateway,
			SecretOutputCh: rt.SecretOutputCh,
			ProviderRunner: rt.ProviderRunner,
		},
		denoPath: binaryPath,
	}
}
