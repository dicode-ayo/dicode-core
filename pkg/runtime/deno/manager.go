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
func (rt *Runtime) NewExecutor(binaryPath string) pkgruntime.Executor {
	return &Runtime{
		parent:         rt,
		registry:       rt.registry,
		secrets:        rt.secrets,
		db:             rt.db,
		log:            rt.log,
		denoPath:       binaryPath,
		secretOutputCh: rt.secretOutputCh,
		providerRunner: rt.providerRunner,
	}
}
