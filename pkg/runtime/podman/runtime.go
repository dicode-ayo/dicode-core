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
// Old dicode-<taskID>:* images (task removed, or Dockerfile changed) are
// reclaimed best-effort by ReclaimOrphanedImages (see imagegc.go), which the
// daemon calls periodically with the registry's current task list.
package podman

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	podmanpkg "github.com/dicode/dicode/pkg/podman"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/containersec"
	"github.com/dicode/dicode/pkg/runtime/imagegc"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Runtime is the ManagedRuntime implementation for Podman.
type Runtime struct {
	reg *registry.Registry
	log *zap.Logger
	// policy is the container host-config security floor (issue #380).
	// The zero value is the strict default: every dangerous escape denied.
	policy containersec.Policy
}

// New creates a Podman Runtime manager.
func New(reg *registry.Registry, log *zap.Logger) *Runtime {
	return &Runtime{reg: reg, log: log}
}

// SetPolicy installs the operator-configured container security policy.
// Call before NewExecutor; executors copy the policy at creation time.
func (rt *Runtime) SetPolicy(p containersec.Policy) { rt.policy = p }

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
	return &executor{podmanPath: binaryPath, reg: rt.reg, log: rt.log, policy: rt.policy}
}

// --- executor ---

type executor struct {
	podmanPath string
	reg        *registry.Registry
	log        *zap.Logger
	policy     containersec.Policy
}

// Execute implements runtime.Executor.
func (e *executor) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	runID := opts.RunID
	result := &pkgruntime.RunResult{RunID: runID}

	cfg := spec.Docker
	containerName := "dicode-" + runID

	// Security floor (issue #380): task.yaml is untrusted input. Reject
	// dangerous host config (host network namespace, dangerous cap_add,
	// isolation-weakening security_opt, bind mounts of sensitive host paths)
	// before any image is built or any container is created, unless the
	// operator opted in via container_security in dicode.yaml.
	if err := containersec.Validate(cfg, e.policy); err != nil {
		_ = e.reg.AppendLog(ctx, runID, "error", err.Error())
		result.Error = err
		return result, nil
	}

	// Resolve the image: build from Dockerfile or pull.
	imageTag := cfg.Image
	if cfg.Build != nil {
		var err error
		imageTag, err = e.buildImage(ctx, spec, runID)
		if err != nil {
			result.Error = err
			return result, nil
		}
	}

	// Task-controlled values are string-concatenated into the podman argv;
	// reject values that could corrupt the invocation (issue #380).
	if err := validateArgvSafety(cfg, imageTag); err != nil {
		_ = e.reg.AppendLog(ctx, runID, "error", err.Error())
		result.Error = err
		return result, nil
	}

	args := e.buildArgs(cfg, imageTag, containerName, runID, spec.ID)

	e.log.Info("podman run",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("image", imageTag),
		zap.String("container", containerName),
	)

	cmd := exec.CommandContext(ctx, e.podmanPath, args...) //nolint:gosec

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.Error = err
		return result, nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		result.Error = err
		return result, nil
	}

	if err := cmd.Start(); err != nil {
		result.Error = fmt.Errorf("start podman: %w", err)
		return result, nil
	}

	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			_ = e.reg.AppendLog(context.Background(), runID, "info", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			e.log.Warn("stdout scanner error", zap.String("run", runID), zap.Error(err))
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			_ = e.reg.AppendLog(context.Background(), runID, "warn", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			e.log.Warn("stderr scanner error", zap.String("run", runID), zap.Error(err))
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
		result.Error = ctx.Err()
	case exitErr != nil:
		result.Error = exitErr
	}

	return result, nil
}

// buildImage builds a Podman image from the task's Dockerfile and returns the image tag.
// Results are cached by Dockerfile content hash — if the image already exists the build is skipped.
func (e *executor) buildImage(ctx context.Context, spec *task.Spec, runID string) (string, error) {
	b := spec.Docker.Build
	dockerfilePath, contextDir := b.ResolvePaths(spec.TaskDir)

	content, err := os.ReadFile(dockerfilePath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read Dockerfile: %w", err)
	}
	tag := imagegc.Tag(spec.ID, content)

	// Cache hit: image with this tag already exists.
	if exec.CommandContext(ctx, e.podmanPath, "image", "exists", tag).Run() == nil { //nolint:gosec
		_ = e.reg.AppendLog(ctx, runID, "info", "image up to date ("+tag+"), skipping build")
		return tag, nil
	}

	_ = e.reg.AppendLog(ctx, runID, "info", "building image "+tag+"…")

	buildCmd := []string{"build", "-t", tag, "-f", dockerfilePath, contextDir}
	cmd := exec.CommandContext(ctx, e.podmanPath, buildCmd...) //nolint:gosec

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("podman build stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("podman build stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("podman build: %w", err)
	}

	buildDone := make(chan struct{})
	go func() {
		defer close(buildDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			_ = e.reg.AppendLog(ctx, runID, "info", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			e.log.Warn("build stdout scanner error", zap.String("run", runID), zap.Error(err))
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			_ = e.reg.AppendLog(ctx, runID, "warn", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			e.log.Warn("build stderr scanner error", zap.String("run", runID), zap.Error(err))
		}
	}()

	<-buildDone
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("podman build failed: %w", err)
	}

	return tag, nil
}

func (e *executor) buildArgs(cfg *task.DockerConfig, imageTag, containerName, runID, taskID string) []string {
	args := []string{
		"run", "--rm",
		"--name", containerName,
		"--label", "dicode.run-id=" + runID,
		"--label", "dicode.task-id=" + taskID,
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
	if cfg.NetworkMode != "" {
		args = append(args, "--network", cfg.NetworkMode)
	}
	for _, h := range cfg.ExtraHosts {
		args = append(args, "--add-host", h)
	}
	for _, c := range cfg.CapDrop {
		args = append(args, "--cap-drop", c)
	}
	for _, c := range cfg.CapAdd {
		args = append(args, "--cap-add", c)
	}
	for _, o := range cfg.SecurityOpt {
		args = append(args, "--security-opt", o)
	}
	if cfg.ReadOnly {
		args = append(args, "--read-only")
	}
	if cfg.User != "" {
		args = append(args, "--user", cfg.User)
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
		ep, _ := json.Marshal(cfg.Entrypoint)
		args = append(args, "--entrypoint", string(ep))
	}
	args = append(args, imageTag)
	args = append(args, cfg.Command...)
	return args
}

// CleanupOrphanedContainers stops and removes any podman containers left
// behind by a previous dicode session (identified by the dicode.run-id label).
func CleanupOrphanedContainers(ctx context.Context, log *zap.Logger) {
	podmanPath, err := podmanpkg.BinaryPath()
	if err != nil {
		log.Debug("podman unavailable, skipping orphan cleanup", zap.Error(err))
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(listCtx, podmanPath, "ps", "-a", //nolint:gosec
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
		rmCtx, rmCancel := context.WithTimeout(ctx, 15*time.Second)
		_ = exec.CommandContext(rmCtx, podmanPath, "rm", "-f", name).Run() //nolint:gosec
		rmCancel()
	}
}
