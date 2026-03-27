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
// Uses the same docker: section in task.yaml as the Docker runtime.
// Either docker.image (pull) or docker.build (local Dockerfile) must be set:
//
//	runtime: podman
//
//	docker:
//	  build:
//	    dockerfile: Dockerfile   # default
//	    context: .               # default: task folder
//	  ports:
//	    - "8888:80"
//
// # Build caching
//
// Images are tagged dicode-<taskID>:<hash> where hash is derived from the
// Dockerfile content. If the image already exists, the build is skipped.
//
// TODO: clean up old dicode-<taskID>:* images when a task is removed or the Dockerfile changes.
package podman

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func (rt *Runtime) DefaultVersion() string { return "" }

func (rt *Runtime) BinaryPath(_ string) (string, error) {
	return podmanpkg.BinaryPath()
}

func (rt *Runtime) IsInstalled(_ string) bool {
	return podmanpkg.IsInstalled()
}

func (rt *Runtime) Install(_ context.Context, _ string) error {
	return fmt.Errorf("podman must be installed via your system package manager — see https://podman.io/docs/installation")
}

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

	// Resolve the image: build from Dockerfile or pull.
	imageTag := cfg.Image
	if cfg.Build != nil {
		var err error
		imageTag, err = e.buildImage(ctx, spec, runID)
		if err != nil {
			if ctx.Err() != nil {
				status = registry.StatusCancelled
			} else {
				status = registry.StatusFailure
			}
			result.Error = err
			return result, nil
		}
	}

	args := e.buildArgs(cfg, imageTag, containerName, spec)

	e.log.Info("podman run",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("image", imageTag),
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

	go func() {
		<-ctx.Done()
		_ = exec.Command(e.podmanPath, "stop", "--time", "10", containerName).Run() //nolint:gosec
		_ = exec.Command(e.podmanPath, "rm", "-f", containerName).Run()             //nolint:gosec
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

	return result, nil
}

// buildImage builds a Podman image from the task's Dockerfile and returns the image tag.
// Results are cached by Dockerfile content hash — if the image already exists the build is skipped.
func (e *executor) buildImage(ctx context.Context, spec *task.Spec, runID string) (string, error) {
	b := spec.Docker.Build

	dockerfilePath := b.Dockerfile
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	if !filepath.IsAbs(dockerfilePath) {
		dockerfilePath = filepath.Join(spec.TaskDir, dockerfilePath)
	}

	contextDir := spec.TaskDir
	if b.Context != "" {
		if filepath.IsAbs(b.Context) {
			contextDir = b.Context
		} else {
			contextDir = filepath.Join(spec.TaskDir, b.Context)
		}
	}

	content, err := os.ReadFile(dockerfilePath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read Dockerfile: %w", err)
	}
	h := sha256.Sum256(content)
	tag := fmt.Sprintf("dicode-%s:%x", spec.ID, h[:6])

	// Cache hit: image with this tag already exists.
	if exec.CommandContext(ctx, e.podmanPath, "image", "exists", tag).Run() == nil { //nolint:gosec
		_ = e.reg.AppendLog(ctx, runID, "info", "image up to date ("+tag+"), skipping build")
		return tag, nil
	}

	_ = e.reg.AppendLog(ctx, runID, "info", "building image "+tag+"…")

	buildArgs := []string{"build", "-t", tag, "-f", dockerfilePath, contextDir}
	cmd := exec.CommandContext(ctx, e.podmanPath, buildArgs...) //nolint:gosec

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("podman build: %w", err)
	}

	buildDone := make(chan struct{})
	go func() {
		defer close(buildDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			_ = e.reg.AppendLog(ctx, runID, "info", scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			_ = e.reg.AppendLog(ctx, runID, "info", scanner.Text())
		}
	}()

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("podman build failed: %w", err)
	}
	<-buildDone

	return tag, nil
}

func (e *executor) buildArgs(cfg *task.DockerConfig, imageTag, containerName string, spec *task.Spec) []string {
	args := []string{
		"run", "--rm",
		"--name", containerName,
		"--label", "dicode.run-id=" + runID(spec, containerName),
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
	// Pull policy only applies when using a pre-built image (not a local build).
	if cfg.Build == nil {
		switch cfg.PullPolicy {
		case "always":
			args = append(args, "--pull=always")
		case "never":
			args = append(args, "--pull=never")
		default:
			args = append(args, "--pull=missing")
		}
	} else {
		args = append(args, "--pull=never") // image was just built locally
	}
	if len(cfg.Entrypoint) > 0 {
		args = append(args, "--entrypoint", strings.Join(cfg.Entrypoint, " "))
	}
	args = append(args, imageTag)
	args = append(args, cfg.Command...)
	return args
}

// runID extracts the run ID from the container name ("dicode-<runID>").
func runID(_ *task.Spec, containerName string) string {
	return strings.TrimPrefix(containerName, "dicode-")
}

// CleanupOrphanedContainers stops and removes any podman containers left
// behind by a previous dicode session (identified by the dicode.run-id label).
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
