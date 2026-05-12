package podman

import (
	"slices"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

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
	args := e.buildArgs(cfg, "cloudflare/cloudflared:latest", "dicode-run1", "run1", "task1")

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
	cfg := &task.DockerConfig{Image: "alpine"}
	e := &executor{podmanPath: "/usr/bin/podman"}
	args := e.buildArgs(cfg, "alpine", "dicode-run1", "run1", "task1")

	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"--network", "--add-host", "--cap-drop", "--cap-add", "--security-opt", "--read-only", "--user"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("unexpected %s in %v", forbidden, args)
		}
	}
}

func TestBuildArgs_HardeningPrecedesImage(t *testing.T) {
	// The image tag must remain the first positional arg after flags, otherwise
	// podman parses subsequent hardening flags as the container's command args.
	cfg := &task.DockerConfig{
		Image:       "alpine",
		NetworkMode: "host",
		ReadOnly:    true,
		Command:     []string{"echo", "hi"},
	}
	e := &executor{podmanPath: "/usr/bin/podman"}
	args := e.buildArgs(cfg, "alpine", "dicode-run1", "run1", "task1")

	imageIdx := -1
	for i, a := range args {
		if a == "alpine" {
			imageIdx = i
			break
		}
	}
	if imageIdx < 0 {
		t.Fatalf("image tag not found in args: %v", args)
	}
	for _, flag := range []string{"--network", "--read-only"} {
		idx := -1
		for i, a := range args {
			if a == flag {
				idx = i
				break
			}
		}
		if idx < 0 || idx >= imageIdx {
			t.Errorf("%s must appear before image tag (at %d); args=%v", flag, imageIdx, args)
		}
	}
	// Command must come after image.
	if args[len(args)-2] != "echo" || args[len(args)-1] != "hi" {
		t.Errorf("command args malformed at tail: %v", args)
	}
}
