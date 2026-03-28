// Package runtime defines the common executor interface and the ManagedRuntime
// interface for runtimes whose interpreter binary is downloaded and cached by
// dicode (e.g. Deno, Python via uv).
package runtime

import "context"

// ManagedRuntime is implemented by runtimes whose interpreter binary is
// downloaded and cached by dicode rather than installed by the OS.
//
// Implementing this interface is all that is needed to make a new managed
// runtime appear in the Runtimes section of the Config UI and participate in
// on-demand installation via the /api/runtimes/{name}/install endpoint.
//
// The concrete implementation should be created in main.go (or equivalent)
// where all dependencies (registry, log, secrets, etc.) are available.
// Those dependencies are captured in the struct at construction time so that
// NewExecutor requires only the binary path.
type ManagedRuntime interface {
	// Name is the runtime identifier used in task.yaml (e.g. "deno", "python").
	Name() string

	// DisplayName is the human-friendly label shown in the Config UI.
	DisplayName() string

	// Description is a one-line description shown in the Config UI.
	Description() string

	// DefaultVersion is the version of the interpreter bundled with this
	// release of dicode. Users can override it in dicode.yaml.
	DefaultVersion() string

	// BinaryPath returns the expected filesystem path for the given version,
	// regardless of whether it is currently installed.
	BinaryPath(version string) (string, error)

	// IsInstalled reports whether the binary for the given version is present.
	IsInstalled(version string) bool

	// Install downloads and caches the interpreter binary for the given
	// version. It is idempotent — calling it when already installed is safe.
	Install(ctx context.Context, version string) error

	// NewExecutor creates a ready-to-use Executor using the interpreter binary
	// at binaryPath. Called after Install completes, or at startup when the
	// binary is already present on disk.
	NewExecutor(binaryPath string) Executor
}
