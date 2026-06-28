package podman

import (
	"slices"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/runtime/containersec"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// TestNewExecutor_PropagatesPolicy pins that the container security policy
// set on the manager reaches the executor created later — including the
// executors re-created by the webui install path (issue #380).
func TestNewExecutor_PropagatesPolicy(t *testing.T) {
	rt := New(nil, zap.NewNop())
	rt.SetPolicy(containersec.Policy{AllowHostNetwork: true, AllowedCapAdd: []string{"SYS_PTRACE"}})
	e, ok := rt.NewExecutor("/usr/bin/podman").(*executor)
	if !ok {
		t.Fatalf("NewExecutor did not return *executor")
	}
	if !e.policy.AllowHostNetwork {
		t.Errorf("AllowHostNetwork not propagated to executor")
	}
	if len(e.policy.AllowedCapAdd) != 1 || e.policy.AllowedCapAdd[0] != "SYS_PTRACE" {
		t.Errorf("AllowedCapAdd not propagated: %v", e.policy.AllowedCapAdd)
	}
}

// argsContainPair asserts that args contains `flag` immediately followed by `value`.
func argsContainPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// argsContainFlag asserts that args contains `flag` as a standalone token (no value).
func argsContainFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

func TestBuildArgs_HardeningFlags(t *testing.T) {
	cfg := &task.DockerConfig{
		Image:       "cloudflare/cloudflared:latest",
		NetworkMode: "bridge",
		ExtraHosts:  []string{"host.docker.internal:host-gateway", "api.local:10.0.0.5"},
		CapDrop:     []string{"ALL"},
		CapAdd:      []string{"NET_BIND_SERVICE"},
		SecurityOpt: []string{"no-new-privileges:true", "label=disable"},
		ReadOnly:    true,
		User:        "65532:65532",
	}
	e := &executor{podmanPath: "/usr/bin/podman"}
	args := e.buildArgs(cfg, "cloudflare/cloudflared:latest", "dicode-run1", "run1", "task1", "bridge")

	if !argsContainPair(args, "--network", "bridge") {
		t.Errorf("missing --network bridge in %v", args)
	}
	if !argsContainPair(args, "--add-host", "host.docker.internal:host-gateway") {
		t.Errorf("missing first --add-host in %v", args)
	}
	if !argsContainPair(args, "--add-host", "api.local:10.0.0.5") {
		t.Errorf("missing second --add-host in %v", args)
	}
	if !argsContainPair(args, "--cap-drop", "ALL") {
		t.Errorf("missing --cap-drop ALL in %v", args)
	}
	if !argsContainPair(args, "--cap-add", "NET_BIND_SERVICE") {
		t.Errorf("missing --cap-add NET_BIND_SERVICE in %v", args)
	}
	if !argsContainPair(args, "--security-opt", "no-new-privileges:true") {
		t.Errorf("missing --security-opt no-new-privileges:true in %v", args)
	}
	if !argsContainPair(args, "--security-opt", "label=disable") {
		t.Errorf("missing --security-opt label=disable in %v", args)
	}
	if !argsContainFlag(args, "--read-only") {
		t.Errorf("missing --read-only in %v", args)
	}
	if !argsContainPair(args, "--user", "65532:65532") {
		t.Errorf("missing --user 65532:65532 in %v", args)
	}
}

func TestBuildArgs_OmitsHardeningWhenUnset(t *testing.T) {
	// When netMode is "" (no explicit mode, no permissions.net, but has ports),
	// buildArgs must not inject any --network flag — the runtime picks its default.
	cfg := &task.DockerConfig{Image: "alpine"}
	e := &executor{podmanPath: "/usr/bin/podman"}
	args := e.buildArgs(cfg, "alpine", "dicode-run1", "run1", "task1", "")

	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"--network", "--add-host", "--cap-drop", "--cap-add", "--security-opt", "--read-only", "--user"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("unexpected %s in %v", forbidden, args)
		}
	}
}

// TestBuildArgs_NetworkNone_ZeroDefaultPerms pins the regression: a task with no
// permissions.net and no ports must receive --network none in the podman argv.
// Before #214 the container would get default (bridge) network regardless.
func TestBuildArgs_NetworkNone_ZeroDefaultPerms(t *testing.T) {
	cfg := &task.DockerConfig{Image: "alpine"}
	e := &executor{podmanPath: "/usr/bin/podman"}
	// EffectiveNetworkMode returns "none" when permissions.net is empty and no ports.
	args := e.buildArgs(cfg, "alpine", "dicode-run1", "run1", "task1", "none")

	if !argsContainPair(args, "--network", "none") {
		t.Errorf("expected --network none for zero-default isolation; args=%v", args)
	}
}

// TestBuildArgs_NetworkBridge_WhenPermissionsNetWildcard pins that a task
// declaring permissions.net: ["*"] does not get --network none injected.
func TestBuildArgs_NetworkBridge_WhenPermissionsNetWildcard(t *testing.T) {
	cfg := &task.DockerConfig{Image: "alpine"}
	e := &executor{podmanPath: "/usr/bin/podman"}
	// EffectiveNetworkMode returns "" when permissions.net is non-empty.
	args := e.buildArgs(cfg, "alpine", "dicode-run1", "run1", "task1", "")

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--network none") {
		t.Errorf("task with permissions.net must not get --network none; args=%v", args)
	}
}

func TestBuildArgs_HardeningPrecedesImage(t *testing.T) {
	// The image tag must remain the first positional arg after flags, otherwise
	// podman parses subsequent hardening flags as the container's command args.
	cfg := &task.DockerConfig{
		Image:       "alpine",
		NetworkMode: "bridge",
		ExtraHosts:  []string{"host.docker.internal:host-gateway"},
		CapDrop:     []string{"ALL"},
		CapAdd:      []string{"NET_BIND_SERVICE"},
		SecurityOpt: []string{"no-new-privileges:true"},
		ReadOnly:    true,
		User:        "65532:65532",
		Command:     []string{"echo", "hi"},
	}
	e := &executor{podmanPath: "/usr/bin/podman"}
	args := e.buildArgs(cfg, "alpine", "dicode-run1", "run1", "task1", "bridge")

	imageIdx := slices.Index(args, "alpine")
	if imageIdx < 0 {
		t.Fatalf("image tag not found in args: %v", args)
	}
	for _, flag := range []string{"--network", "--add-host", "--cap-drop", "--cap-add", "--security-opt", "--read-only", "--user"} {
		idx := slices.Index(args, flag)
		if idx < 0 {
			t.Errorf("%s missing from args: %v", flag, args)
			continue
		}
		if idx >= imageIdx {
			t.Errorf("%s must appear before image tag (at %d); args=%v", flag, imageIdx, args)
		}
	}
	// Command must come after image.
	if args[len(args)-2] != "echo" || args[len(args)-1] != "hi" {
		t.Errorf("command args malformed at tail: %v", args)
	}
}

// TestValidateArgvSafety_Rejections pins the argv-injection floor (issue
// #380): task-controlled values that could corrupt the podman invocation are
// rejected before buildArgs assembles the argv.
func TestValidateArgvSafety_Rejections(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *task.DockerConfig
		imageRef string
	}{
		{
			name:     "empty image ref",
			cfg:      &task.DockerConfig{},
			imageRef: "",
		},
		{
			name:     "image ref starting with dash is parsed as a flag",
			cfg:      &task.DockerConfig{},
			imageRef: "--privileged",
		},
		{
			name:     "image ref with embedded space",
			cfg:      &task.DockerConfig{},
			imageRef: "alpine --privileged",
		},
		{
			name:     "image ref with newline",
			cfg:      &task.DockerConfig{},
			imageRef: "alpine\nlatest",
		},
		{
			name:     "port with newline",
			cfg:      &task.DockerConfig{Ports: []string{"8080:80\n"}},
			imageRef: "alpine",
		},
		{
			name:     "volume with NUL byte",
			cfg:      &task.DockerConfig{Volumes: []string{"/srv/a\x00b:/data"}},
			imageRef: "alpine",
		},
		{
			name:     "extra host with carriage return",
			cfg:      &task.DockerConfig{ExtraHosts: []string{"evil:1.2.3.4\r"}},
			imageRef: "alpine",
		},
		{
			name:     "network mode with newline",
			cfg:      &task.DockerConfig{NetworkMode: "bridge\n"},
			imageRef: "alpine",
		},
		{
			name:     "user with control character",
			cfg:      &task.DockerConfig{User: "0:0\n"},
			imageRef: "alpine",
		},
		{
			name:     "env key with equals smuggles a second pair",
			cfg:      &task.DockerConfig{EnvVars: map[string]string{"FOO=BAR": "x"}},
			imageRef: "alpine",
		},
		{
			name:     "env key with leading dash",
			cfg:      &task.DockerConfig{EnvVars: map[string]string{"-e": "x"}},
			imageRef: "alpine",
		},
		{
			name:     "env value with NUL",
			cfg:      &task.DockerConfig{EnvVars: map[string]string{"FOO": "a\x00b"}},
			imageRef: "alpine",
		},
		{
			name:     "command token with NUL",
			cfg:      &task.DockerConfig{Command: []string{"echo", "a\x00b"}},
			imageRef: "alpine",
		},
		{
			name:     "entrypoint token with NUL",
			cfg:      &task.DockerConfig{Entrypoint: []string{"/bin/sh\x00"}},
			imageRef: "alpine",
		},
		{
			name:     "security opt with newline",
			cfg:      &task.DockerConfig{SecurityOpt: []string{"no-new-privileges:true\n"}},
			imageRef: "alpine",
		},
		{
			name:     "cap_add with newline",
			cfg:      &task.DockerConfig{CapAdd: []string{"NET_BIND_SERVICE\n"}},
			imageRef: "alpine",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateArgvSafety(tt.cfg, tt.imageRef); err == nil {
				t.Errorf("expected rejection for %s", tt.name)
			}
		})
	}
}

func TestValidateArgvSafety_SafeConfigPasses(t *testing.T) {
	cfg := &task.DockerConfig{
		Ports:       []string{"8080:80", "9090:90/udp"},
		Volumes:     []string{"/srv/data:/data:ro", "named-vol:/cache"},
		ExtraHosts:  []string{"host.docker.internal:host-gateway"},
		EnvVars:     map[string]string{"FOO": "bar baz", "MULTI": "line1\nline2", "_UNDERSCORE": "ok"},
		WorkingDir:  "/app",
		NetworkMode: "bridge",
		CapDrop:     []string{"ALL"},
		CapAdd:      []string{"NET_BIND_SERVICE"},
		SecurityOpt: []string{"no-new-privileges:true"},
		User:        "65532:65532",
		Command:     []string{"echo", "--flag-for-the-container", "hi"},
		Entrypoint:  []string{"/bin/sh", "-c"},
	}
	if err := validateArgvSafety(cfg, "alpine:3.20"); err != nil {
		t.Errorf("safe config rejected: %v", err)
	}
	// Built image tags look like dicode-<task>:<hash> and must pass too.
	if err := validateArgvSafety(&task.DockerConfig{}, "dicode-mytask:a1b2c3d4e5f6"); err != nil {
		t.Errorf("built image tag rejected: %v", err)
	}
}
