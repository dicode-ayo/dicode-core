// Package docker executes tasks by running Docker containers.
// Logs are streamed live to the run log as the container produces output.
// A task context cancellation stops the container gracefully (SIGTERM + 10s).
package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/containersec"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/runtime/imagegc"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	dockerbuild "github.com/docker/docker/api/types/build"
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
	// PreResolvedEnv, when set, carries the task's declared permissions.env
	// after the trigger engine's resolver produced values (including
	// secret-store values). Its .Env is merged into the container env and
	// its .Secrets feed the run-log redactor. Nil leaves the container with
	// only the literal docker.env_vars.
	PreResolvedEnv *envresolve.Resolved
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
	// policy is the container host-config security floor (issue #380).
	// The zero value is the strict default: every dangerous escape denied.
	policy containersec.Policy
}

// New creates a Docker Runtime.
func New(r *registry.Registry, log *zap.Logger) *Runtime {
	return &Runtime{registry: r, log: log}
}

// SetPolicy installs the operator-configured container security policy.
// Call before the first Run; without it the strict default applies.
func (rt *Runtime) SetPolicy(p containersec.Policy) { rt.policy = p }

// fail marks a run as failed and returns the result.
func (rt *Runtime) fail(runID string, err error) (*RunResult, error) {
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

	// PreResolvedEnv is already resolved by the trigger engine, so derive the
	// env map and redactor directly — no resolver is constructed here.
	var resolvedEnv map[string]string
	var redactor *secrets.Redactor
	if opts.PreResolvedEnv != nil {
		resolvedEnv = opts.PreResolvedEnv.Env
		redactor = secrets.NewRedactor(opts.PreResolvedEnv.Secrets)
	}

	// Security floor (issue #380): task.yaml is untrusted input. Reject
	// dangerous host config (host network namespace, dangerous cap_add,
	// isolation-weakening security_opt, bind mounts of sensitive host paths)
	// before any image is built or any container is created, unless the
	// operator opted in via container_security in dicode.yaml.
	if err := containersec.Validate(cfg, rt.policy); err != nil {
		_ = rt.registry.AppendLog(ctx, runID, "error", err.Error())
		return rt.fail(runID, err)
	}

	// Zero-default network isolation (#214): when no docker.network_mode is
	// declared and the task has no network permissions and publishes no ports,
	// default to "none" to deny all outbound connectivity.
	effectiveNetMode := pkgruntime.EffectiveNetworkMode(cfg.NetworkMode, spec.Permissions, cfg.Ports)
	if pkgruntime.NetPermsNeedWarning(cfg.NetworkMode, spec.Permissions) {
		_ = rt.registry.AppendLog(ctx, runID, "warn",
			"permissions.net lists specific hosts but per-host network enforcement is not yet implemented for Docker — outbound network is unrestricted; use docker.network_mode: none to deny all, or [\"*\"] to grant unrestricted access explicitly")
	}
	if effectiveNetMode == "none" && len(cfg.ExtraHosts) > 0 {
		_ = rt.registry.AppendLog(ctx, runID, "warn",
			"docker.extra_hosts are defined but the container has network_mode: none — extra host entries will be unreachable; set permissions.net or docker.network_mode to enable network access")
	}

	// Resolve the image: build from Dockerfile or pull.
	imageTag := cfg.Image
	if cfg.Build != nil {
		imageTag, err = rt.buildImage(ctx, dc, spec, runID, effectiveNetMode)
		if err != nil {
			if ctx.Err() != nil {
				return &RunResult{RunID: runID, Error: err}, nil
			}
			return rt.fail(runID, err)
		}
	} else {
		pullPolicy := cfg.PullPolicy
		if pullPolicy == "" {
			pullPolicy = "missing"
		}
		if err := rt.maybePull(ctx, dc, cfg.Image, pullPolicy, runID); err != nil {
			if ctx.Err() != nil {
				return &RunResult{RunID: runID, Error: err}, nil
			}
			return rt.fail(runID, err)
		}
	}

	// Build container config.
	containerCfg := &container.Config{
		Image: imageTag,
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
	if cfg.User != "" {
		containerCfg.User = cfg.User
	}
	containerCfg.Env = pkgruntime.BuildContainerEnv(cfg.EnvVars, resolvedEnv)

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
		Binds:          cfg.Volumes,
		PortBindings:   portBindings,
		AutoRemove:     false,
		NetworkMode:    container.NetworkMode(effectiveNetMode),
		ExtraHosts:     cfg.ExtraHosts,
		CapAdd:         cfg.CapAdd,
		CapDrop:        cfg.CapDrop,
		SecurityOpt:    cfg.SecurityOpt,
		ReadonlyRootfs: cfg.ReadOnly,
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
		zap.String("image", imageTag),
	)

	defer func() {
		_ = dc.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true})
	}()

	if err := dc.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return rt.fail(runID, fmt.Errorf("start container: %w", err))
	}

	waitStatusCh, waitErrCh := dc.ContainerWait(context.Background(), containerID, container.WaitConditionNotRunning)

	logReader, err := dc.ContainerLogs(context.Background(), containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: false,
	})
	if err != nil {
		return rt.fail(runID, fmt.Errorf("attach logs: %w", err))
	}

	var closeOnce sync.Once
	closeLog := func() { closeOnce.Do(func() { logReader.Close() }) }
	defer closeLog()

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
		rt.streamLines(runID, stdoutR, "info", redactor)
	}()
	go func() { rt.streamLines(runID, stderrR, "warn", redactor) }()

	go func() {
		<-ctx.Done()
		closeLog()
		stopTimeout := 10
		_ = dc.ContainerStop(context.Background(), containerID, container.StopOptions{Timeout: &stopTimeout})
	}()

	var exitCode int64
	select {
	case waitResult := <-waitStatusCh:
		exitCode = waitResult.StatusCode
	case waitErr := <-waitErrCh:
		result.Error = fmt.Errorf("container wait: %w", waitErr)
	}

	// Do NOT force-close the log reader here. When the container exits, Docker
	// closes the follow stream naturally, which drains stdcopy and the scanners.
	// Force-closing immediately after ContainerWait races with buffered log data
	// still in flight from the daemon — fast containers lose their last log lines.
	// The kill-watcher goroutine handles the cancellation path via closeLog().
	<-logDone
	closeLog() // no-op if the kill-watcher already closed it

	if result.Error == nil && exitCode != 0 {
		result.Error = fmt.Errorf("container exited with code %d", exitCode)
	}
	return result, nil
}

// buildImage builds a Docker image from the task's Dockerfile and returns the image tag.
// Results are cached by Dockerfile content hash — if the Dockerfile hasn't changed the
// existing image is reused and the build is skipped entirely.
// netMode is applied to the build's RUN steps so that Dockerfile layers respect the
// same network isolation policy as the eventual container run.
func (rt *Runtime) buildImage(ctx context.Context, dc *dockerclient.Client, spec *task.Spec, runID, netMode string) (string, error) {
	b := spec.Docker.Build
	dockerfilePath, contextDir := b.ResolvePaths(spec.TaskDir)

	content, err := os.ReadFile(dockerfilePath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read Dockerfile: %w", err)
	}
	// Include netMode in the cache key: a build with network access must not be
	// reused for a task that later restricts to network_mode: none (or vice versa).
	cacheMaterial := append([]byte{}, content...)
	cacheMaterial = append(cacheMaterial, []byte("\x00dicode-build-network-mode:"+netMode)...)
	tag := imagegc.Tag(spec.ID, cacheMaterial)

	// Cache hit: image with this tag already exists.
	if _, _, err := dc.ImageInspectWithRaw(ctx, tag); err == nil {
		_ = rt.registry.AppendLog(ctx, runID, "info", "image up to date ("+tag+"), skipping build")
		return tag, nil
	}

	_ = rt.registry.AppendLog(ctx, runID, "info", "building image "+tag+"…")

	relDockerfile, err := filepath.Rel(contextDir, dockerfilePath)
	if err != nil {
		relDockerfile = "Dockerfile"
	}

	buildCtx, err := buildContextTar(contextDir)
	if err != nil {
		return "", fmt.Errorf("create build context: %w", err)
	}

	resp, err := dc.ImageBuild(ctx, buildCtx, dockerbuild.ImageBuildOptions{
		Tags:        []string{tag},
		Dockerfile:  relDockerfile,
		Remove:      true,
		NetworkMode: netMode,
	})
	if err != nil {
		return "", fmt.Errorf("image build: %w", err)
	}
	defer resp.Body.Close()

	var msg struct {
		Stream string `json:"stream"`
		Error  string `json:"error"`
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Error != "" {
			return "", fmt.Errorf("build error: %s", msg.Error)
		}
		if out := strings.TrimSpace(msg.Stream); out != "" {
			_ = rt.registry.AppendLog(ctx, runID, "info", out)
		}
	}
	if err := scanner.Err(); err != nil {
		rt.log.Warn("build output scanner error", zap.String("run", runID), zap.Error(err))
	}

	return tag, nil
}

// buildContextTar creates an uncompressed tar archive of dir for use as a Docker build context.
func buildContextTar(dir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.IsDir() {
			if err := func() error {
				f, err := os.Open(path) //nolint:gosec
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = io.Copy(tw, f)
				return err
			}(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = tw.Close()
	return &buf, nil
}

// maybePull pulls the image according to the pull policy.
func (rt *Runtime) maybePull(ctx context.Context, dc *dockerclient.Client, img, policy, runID string) error {
	switch policy {
	case "never":
		return nil
	case "missing":
		_, _, err := dc.ImageInspectWithRaw(ctx, img)
		if err == nil {
			return nil
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
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if err := json.Unmarshal([]byte(line), &pullMsg); err == nil && pullMsg.Status != "" {
			_ = rt.registry.AppendLog(ctx, runID, "info", pullMsg.Status)
		}
	}
	if err := scanner.Err(); err != nil {
		rt.log.Warn("image pull scanner error", zap.String("run", runID), zap.Error(err))
	}
	return nil
}

// streamLines reads lines from r and appends them to the run log, redacting
// resolved secret values first. A nil redactor is a passthrough.
func (rt *Runtime) streamLines(runID string, r io.Reader, level string, redactor *secrets.Redactor) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		_ = rt.registry.AppendLog(context.Background(), runID, level, redactor.RedactString(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		rt.log.Warn("stream scanner error", zap.String("run", runID), zap.Error(err))
	}
}

// Execute implements runtime.Executor.
func (rt *Runtime) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	result, err := rt.Run(ctx, spec, RunOptions{
		RunID:          opts.RunID,
		ParentRunID:    opts.ParentRunID,
		PreResolvedEnv: opts.PreResolvedEnv,
	})
	if err != nil {
		return nil, err
	}
	return &pkgruntime.RunResult{RunID: result.RunID, Error: result.Error}, nil
}
