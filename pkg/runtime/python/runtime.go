// Package python executes Python task scripts via the managed uv binary.
//
// uv (https://github.com/astral-sh/uv) is a fast Python package manager and
// script runner that dicode downloads and caches automatically — no system
// Python or pip installation is required.
//
// # Execution model
//
//	<uv-binary> run task.py
//
// uv creates a per-script virtual environment on first run and caches it for
// subsequent runs. Inline dependency declarations (PEP 723) are supported:
//
//	# /// script
//	# dependencies = ["requests>=2.31", "boto3"]
//	# ///
//	import requests
//
// # SDK / params
//
// Python tasks receive run context via environment variables:
//
//	DICODE_RUN_ID            — the current run ID
//	DICODE_PARAM_<NAME>      — value of each task parameter (name uppercased)
//
// Example:
//
//	import os
//	channel = os.environ.get("DICODE_PARAM_SLACK_CHANNEL", "#general")
//
// Env vars declared in task.yaml under `env:` are inherited from the host
// process; resolved secrets must be present in the host environment.
//
// Stdout is captured as info-level log entries; stderr as warn-level entries.
package python

import (
	"context"
	"os"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/subprocess"
	uvpkg "github.com/dicode/dicode/pkg/uv"
	"go.uber.org/zap"
)

// Runtime is the ManagedRuntime implementation for Python+uv.
// It manages the uv binary lifecycle and creates subprocess Executors.
// It does NOT itself implement Executor; use NewExecutor(binaryPath) to obtain
// a configured, ready-to-use Executor.
type Runtime struct {
	reg *registry.Registry
	log *zap.Logger
}

// New creates a Python Runtime manager pre-configured with the registry and
// logger that every created Executor will share.
func New(reg *registry.Registry, log *zap.Logger) *Runtime {
	return &Runtime{reg: reg, log: log}
}

// --- ManagedRuntime interface ---

func (rt *Runtime) Name() string        { return "python" }
func (rt *Runtime) DisplayName() string { return "Python (uv)" }
func (rt *Runtime) Description() string {
	return "Python runtime managed by uv. Supports inline dependencies via PEP 723 (# /// script blocks). Params available as DICODE_PARAM_* env vars."
}
func (rt *Runtime) DefaultVersion() string { return uvpkg.DefaultVersion }

// BinaryPath returns the expected cache path for the uv binary at the given version.
func (rt *Runtime) BinaryPath(version string) (string, error) {
	return uvpkg.BinaryPath(version)
}

// IsInstalled reports whether the uv binary for the given version is cached.
func (rt *Runtime) IsInstalled(version string) bool {
	p, err := uvpkg.BinaryPath(version)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Install downloads and caches the uv binary for the given version.
func (rt *Runtime) Install(_ context.Context, version string) error {
	_, err := uvpkg.EnsureUv(version)
	return err
}

// NewExecutor returns a subprocess.Executor that runs Python scripts via the
// uv binary at binaryPath.
func (rt *Runtime) NewExecutor(binaryPath string) pkgruntime.Executor {
	// "uv run task.py" — uv manages venv creation and dependency installation.
	return subprocess.New(binaryPath, []string{"run"}, ".py", rt.reg, rt.log)
}
