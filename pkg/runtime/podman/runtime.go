// Package podman executes tasks by running Podman containers via the CLI.
//
// Unlike the Docker runtime (which uses the Docker Go SDK), this runtime
// shells out to the podman binary. This means:
//   - No daemon required — podman runs rootless by default.
//   - Stdout and stderr are plain text streams (no Docker multiplexing).
//   - Podman must be installed on the host via the system package manager.
//
// # Task spec
//
// Uses the same docker: section in task.yaml as the Docker runtime:
//
//	runtime: podman
//
//	docker:
//	  image: nginx:alpine
//	  ports:
//	    - "8888:80"
//	  volumes:
//	    - "/tmp:/usr/share/nginx/html:ro"
//
// # Orphan cleanup
//
// Containers are named "dicode-<runID>" so they can be found and removed on
// the next startup if dicode was killed without a clean shutdown.
package podman

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	podmanpkg "github.com/dicode/dicode/pkg/podman"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Runtime is the ManagedRuntime implementation for Podman.
type Runtime struct {
	reg *registry.Registry
	log *zap.Logger
}

// New creates a Podman Runtime manager.
func New(reg *registry.Registry, log *zap.Logger) *Runtime {
	return &Runtime{reg: reg, log: log}
}

// --- ManagedRuntime interface ---

func (rt *Runtime) Name() string        { return "podman" }
func (rt *Runtime) DisplayName() string { return "Podman" }
func (rt *Runtime) Description() string {
	return "Rootless container runtime. Uses the system podman binary — install via your package manager (dnf, apt, brew)."
}

// DefaultVersion returns "" — podman is a system package with no managed version.
func (rt *Runtime) DefaultVersion() string { return "" }

// BinaryPath returns the path to the system podman binary.
// The version argument is ignored; podman is not version-managed by dicode.
func (rt *Runtime) BinaryPath(_ string) (string, error) {
	return podmanpkg.BinaryPath()
}

// IsInstalled reports whether podman is available on the system.
func (rt *Runtime) IsInstalled(_ string) bool {
	return podmanpkg.IsInstalled()
}

// Install is not supported for Podman — it must be installed via the system
// package manager. This method always returns a descriptive error.
func (rt *Runtime) Install(_ context.Context, _ string) error {
	return fmt.Errorf("podman must be installed via your system package manager — see https://podman.io/docs/installation")
}

// NewExecutor returns an Executor that runs containers via the podman binary
// at binaryPath.
func (rt *Runtime) NewExecutor(binaryPath string) pkgruntime.Executor {
	return &executor{podmanPath: binaryPath, reg: rt.reg, log: rt.log}
}

// --- executor ---

type executor struct {
	podmanPath string
	reg        *registry.Registry
	log        *zap.Logger
}

// Execute implements runtime.Executor.
func (e *executor) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	runID := opts.RunID
	result := &pkgruntime.RunResult{RunID: runID}
	status := registry.StatusSuccess

	defer func() {
		if ferr := e.reg.FinishRun(context.Background(), runID, status); ferr != nil {
			e.log.Error("finish run", zap.String("run", runID), zap.Error(ferr))
		}
	}()

	cfg := spec.Docker
	containerName := "dicode-" + runID

	args := e.buildArgs(cfg, containerName, spec)

	e.log.Info("podman run",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("image", cfg.Image),
		zap.String("container", containerName),
	)

	cmd := exec.CommandContext(ctx, e.podmanPath, args...) //nolint:gosec

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}

	if err := cmd.Start(); err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("start podman: %w", err)
		return result, nil
	}

	// Stream stdout (info) and stderr (warn) to the run log concurrently.
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			_ = e.reg.AppendLog(context.Background(), runID, "info", scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			_ = e.reg.AppendLog(context.Background(), runID, "warn", scanner.Text())
		}
	}()

	// When context is cancelled, stop the container gracefully.
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		<-ctx.Done()
		stopCtx := context.Background()
		stopArgs := []string{"stop", "--time", "10", containerName}
		_ = exec.Command(e.podmanPath, stopArgs...).Run() //nolint:gosec
		_ = exec.CommandContext(stopCtx, e.podmanPath, "rm", "-f", containerName).Run() //nolint:gosec
	}()

	exitErr := cmd.Wait()
	<-logDone

	switch {
	case ctx.Err() != nil:
		status = registry.StatusCancelled
		result.Error = ctx.Err()
	case exitErr != nil:
		status = registry.StatusFailure
		result.Error = exitErr
	}

	e.log.Info("podman finished",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("status", status),
	)
	return result, nil
}

func (e *executor) buildArgs(cfg *task.DockerConfig, containerName string, spec *task.Spec) []string {
	args := []string{
		"run",
		"--rm",
		"--name", containerName,
		"--label", "dicode.run-id=" + spec.ID, // task ID in label for cleanup
		"--label", "dicode.task-id=" + spec.ID,
	}

	for _, p := range cfg.Ports {
		args = append(args, "-p", p)
	}
	for _, v := range cfg.Volumes {
		args = append(args, "-v", v)
	}
	for k, v := range cfg.EnvVars {
		args = append(args, "-e", k+"="+v)
	}
	if cfg.WorkingDir != "" {
		args = append(args, "--workdir", cfg.WorkingDir)
	}

	pullPolicy := cfg.PullPolicy
	if pullPolicy == "" {
		pullPolicy = "missing"
	}
	switch pullPolicy {
	case "always":
		args = append(args, "--pull=always")
	case "never":
		args = append(args, "--pull=never")
	default:
		args = append(args, "--pull=missing")
	}

	if len(cfg.Entrypoint) > 0 {
		args = append(args, "--entrypoint", strings.Join(cfg.Entrypoint, " "))
	}

	args = append(args, cfg.Image)

	args = append(args, cfg.Command...)

	return args
}

// CleanupOrphanedContainers stops and removes any podman containers left
// behind by a previous dicode session (identified by the dicode.run-id label).
// Safe to call even when podman is not installed.
func CleanupOrphanedContainers(ctx context.Context, log *zap.Logger) {
	podmanPath, err := podmanpkg.BinaryPath()
	if err != nil {
		log.Debug("podman unavailable, skipping orphan cleanup", zap.Error(err))
		return
	}

	out, err := exec.CommandContext(ctx, podmanPath, "ps", "-a", //nolint:gosec
		"--filter", "label=dicode.run-id",
		"--format", "{{.Names}}",
	).Output()
	if err != nil || len(out) == 0 {
		return
	}

	names := strings.Fields(strings.TrimSpace(string(out)))
	if len(names) == 0 {
		return
	}

	log.Info("removing orphaned podman containers from previous session", zap.Int("count", len(names)))
	for _, name := range names {
		log.Info("removing orphaned container", zap.String("container", name))
		_ = exec.CommandContext(ctx, podmanPath, "rm", "-f", name).Run() //nolint:gosec
	}
}
