// Package docker executes tasks by running Docker containers.
// Logs are streamed live to the run log as the container produces output.
// A task context cancellation stops the container gracefully (SIGTERM + 10s).
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"go.uber.org/zap"
)

// RunOptions controls a single Docker task execution.
type RunOptions struct {
	RunID       string
	ParentRunID string
	Params      map[string]string
}

// RunResult is returned by Run.
type RunResult struct {
	RunID string
	Error error
}

// Runtime executes tasks as Docker containers.
type Runtime struct {
	registry *registry.Registry
	log      *zap.Logger
}

// New creates a Docker Runtime.
func New(r *registry.Registry, log *zap.Logger) *Runtime {
	return &Runtime{registry: r, log: log}
}

// fail marks a run as failed and returns the result. Used to DRY up early-exit error paths.
func (rt *Runtime) fail(runID string, err error) (*RunResult, error) {
	_ = rt.registry.FinishRun(context.Background(), runID, registry.StatusFailure)
	return &RunResult{RunID: runID, Error: err}, nil
}

// Run starts a container for the given spec and streams its logs until it exits or ctx is cancelled.
func (rt *Runtime) Run(ctx context.Context, spec *task.Spec, opts RunOptions) (*RunResult, error) {
	if opts.RunID == "" {
		return nil, fmt.Errorf("docker runtime: RunID must be set by caller")
	}
	runID := opts.RunID
	result := &RunResult{RunID: runID}

	dc, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	defer dc.Close()

	cfg := spec.Docker

	// Pull image if needed.
	pullPolicy := cfg.PullPolicy
	if pullPolicy == "" {
		pullPolicy = "missing"
	}
	if err := rt.maybePull(ctx, dc, cfg.Image, pullPolicy, runID); err != nil {
		if ctx.Err() != nil {
			_ = rt.registry.FinishRun(context.Background(), runID, registry.StatusCancelled)
			return &RunResult{RunID: runID, Error: err}, nil
		}
		return rt.fail(runID, err)
	}

	// Build container config.
	// Labels let startup cleanup identify orphaned containers from a previous session.
	containerCfg := &container.Config{
		Image: cfg.Image,
		Labels: map[string]string{
			"dicode.run-id":  runID,
			"dicode.task-id": spec.ID,
		},
	}
	if len(cfg.Command) > 0 {
		containerCfg.Cmd = cfg.Command
	}
	if len(cfg.Entrypoint) > 0 {
		containerCfg.Entrypoint = cfg.Entrypoint
	}
	if cfg.WorkingDir != "" {
		containerCfg.WorkingDir = cfg.WorkingDir
	}
	var envList []string
	for k, v := range cfg.EnvVars {
		envList = append(envList, k+"="+v)
	}
	containerCfg.Env = envList

	// Parse port bindings: "hostPort:containerPort[/proto]"
	portBindings := nat.PortMap{}
	exposedPorts := nat.PortSet{}
	for _, p := range cfg.Ports {
		parts := strings.SplitN(p, ":", 2)
		if len(parts) != 2 {
			continue
		}
		hostPort, containerPortStr := parts[0], parts[1]
		containerPort := nat.Port(containerPortStr)
		if !strings.Contains(containerPortStr, "/") {
			containerPort = nat.Port(containerPortStr + "/tcp")
		}
		exposedPorts[containerPort] = struct{}{}
		portBindings[containerPort] = []nat.PortBinding{{HostPort: hostPort}}
	}
	containerCfg.ExposedPorts = exposedPorts

	hostCfg := &container.HostConfig{
		Binds:        cfg.Volumes,
		PortBindings: portBindings,
		AutoRemove:   false,
	}

	created, err := dc.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return rt.fail(runID, fmt.Errorf("create container: %w", err))
	}
	containerID := created.ID
	shortID := containerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	rt.log.Info("container created",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("container", shortID),
		zap.String("image", cfg.Image),
	)

	defer func() {
		_ = dc.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true})
	}()

	if err := dc.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return rt.fail(runID, fmt.Errorf("start container: %w", err))
	}
	rt.log.Info("container started",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("container", shortID),
	)

	// ContainerWait is started early so we don't miss the exit event.
	waitStatusCh, waitErrCh := dc.ContainerWait(context.Background(), containerID, container.WaitConditionNotRunning)

	// Attach log stream using context.Background() so we control closure explicitly.
	// This prevents the Docker HTTP transport from leaving reads half-open when the
	// run context is cancelled — we close logReader ourselves to unblock stdcopy.
	logReader, err := dc.ContainerLogs(context.Background(), containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: false,
	})
	if err != nil {
		return rt.fail(runID, fmt.Errorf("attach logs: %w", err))
	}

	// closeLog closes logReader exactly once — safe to call from multiple goroutines.
	var closeOnce sync.Once
	closeLog := func() { closeOnce.Do(func() { logReader.Close() }) }
	defer closeLog()

	// stdcopy demuxes the Docker multiplexed stream into stdout/stderr pipes.
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go func() {
		_, _ = stdcopy.StdCopy(stdoutW, stderrW, logReader)
		stdoutW.Close()
		stderrW.Close()
	}()

	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		rt.streamLines(runID, stdoutR, "info")
	}()
	go func() { rt.streamLines(runID, stderrR, "error") }()

	// Kill watcher: when ctx is cancelled, close the log stream immediately so
	// stdcopy unblocks, then stop the container.
	go func() {
		<-ctx.Done()
		closeLog() // unblocks stdcopy.StdCopy → pipes drain → logDone closed
		stopTimeout := 10
		_ = dc.ContainerStop(context.Background(), containerID, container.StopOptions{Timeout: &stopTimeout})
	}()

	// Wait for the container to exit.
	var exitCode int64
	select {
	case waitResult := <-waitStatusCh:
		exitCode = waitResult.StatusCode
	case waitErr := <-waitErrCh:
		result.Error = fmt.Errorf("container wait: %w", waitErr)
	}

	// Close log reader so stdcopy returns (no-op if kill watcher already did it).
	closeLog()
	// Drain remaining log lines.
	<-logDone

	finalStatus := registry.StatusSuccess
	if ctx.Err() != nil {
		finalStatus = registry.StatusCancelled
	} else if result.Error != nil || exitCode != 0 {
		finalStatus = registry.StatusFailure
		if result.Error == nil {
			result.Error = fmt.Errorf("container exited with code %d", exitCode)
		}
	}
	rt.log.Info("container finished",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("container", shortID),
		zap.Int64("exit_code", exitCode),
		zap.String("status", finalStatus),
	)
	_ = rt.registry.FinishRun(context.Background(), runID, finalStatus)
	return result, nil
}

// maybePull pulls the image according to the pull policy.
func (rt *Runtime) maybePull(ctx context.Context, dc *dockerclient.Client, img, policy, runID string) error {
	switch policy {
	case "never":
		return nil
	case "missing":
		_, _, err := dc.ImageInspectWithRaw(ctx, img)
		if err == nil {
			return nil // already present
		}
	}
	_ = rt.registry.AppendLog(ctx, runID, "info", "pulling image: "+img)
	reader, err := dc.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", img, err)
	}
	defer reader.Close()
	var pullMsg struct {
		Status string `json:"status"`
	}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if err := json.Unmarshal([]byte(line), &pullMsg); err == nil && pullMsg.Status != "" {
			_ = rt.registry.AppendLog(ctx, runID, "info", pullMsg.Status)
		}
	}
	return nil
}

// streamLines reads lines from r and appends them to the run log with the given level.
func (rt *Runtime) streamLines(runID string, r io.Reader, level string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		_ = rt.registry.AppendLog(context.Background(), runID, level, scanner.Text())
	}
}
