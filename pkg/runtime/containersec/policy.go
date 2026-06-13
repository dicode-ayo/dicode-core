// Package containersec enforces a security floor on container host
// configuration coming from untrusted task.yaml files (issue #380).
//
// Task specs live in watched git repos: anyone who can push a task can
// request arbitrary host config. PR #296 made network_mode, cap_add,
// security_opt, volumes, read_only and user configurable per task; without
// a floor, a malicious task escapes to the host trivially — bind-mount /,
// mount the docker/podman control socket, join the host network namespace,
// add CAP_SYS_ADMIN, or disable seccomp/AppArmor/SELinux.
//
// Validate rejects such configs before container creation, in both the
// docker and podman runtimes. The zero-value Policy denies everything
// dangerous (default-deny); operators opt in explicitly via the
// container_security block in dicode.yaml (see pkg/config).
package containersec

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dicode/dicode/pkg/task"
)

// Policy describes which dangerous container host-config escapes an
// operator has explicitly allowed. The zero value denies everything.
type Policy struct {
	// AllowHostNetwork permits network_mode values that join the host's (or
	// another container's) network namespace: "host", "container:<id>",
	// "ns:<path>".
	AllowHostNetwork bool

	// AllowInsecureSecurityOpt permits security_opt entries that disable a
	// kernel sandbox layer: seccomp=unconfined, apparmor=unconfined,
	// label=disable, systempaths=unconfined, unmask=…
	AllowInsecureSecurityOpt bool

	// AllowedCapAdd lists Linux capabilities (case-insensitive, with or
	// without the CAP_ prefix) that tasks may cap_add even though they are
	// on the dangerous list. "ALL" permits every capability, including
	// cap_add: ["ALL"] itself.
	AllowedCapAdd []string

	// AllowedVolumeRoots lists host directories under which bind-mount
	// sources are allowed. When non-empty the policy switches to strict
	// allowlist mode: every bind-mount source must resolve — after symlink
	// and ".." cleaning — inside one of these roots (an explicitly listed
	// root overrides the built-in denylist). When empty (the default),
	// bind mounts are allowed except for the built-in sensitive-path
	// denylist: /, /proc, /sys, /etc, /dev, /boot, /root, /run, /var/run,
	// container-runtime state dirs, and the docker/podman control sockets.
	AllowedVolumeRoots []string
}

// dangerousCapAdd lists capabilities that enable container escape or host
// tampering. Names are normalized (upper-case, no CAP_ prefix).
var dangerousCapAdd = map[string]bool{
	"ALL":             true,
	"SYS_ADMIN":       true, // mount, namespaces — the classic escape cap
	"SYS_PTRACE":      true, // trace/inject into host pids
	"SYS_MODULE":      true, // load kernel modules
	"SYS_RAWIO":       true, // raw device access
	"SYS_BOOT":        true, // reboot the host
	"SYS_TIME":        true, // change host clock
	"NET_ADMIN":       true, // reconfigure host networking (with host netns)
	"DAC_READ_SEARCH": true, // bypass read permission checks (open_by_handle_at escape)
	"DAC_OVERRIDE":    true, // bypass file permission checks
	"BPF":             true, // load eBPF programs
	"SYSLOG":          true, // read kernel logs (kaslr leaks)
}

// sensitiveVolumeRoots are host paths (and everything under them) that must
// never be bind-mounted into a task container unless the operator lists an
// explicit allowed root that covers them.
var sensitiveVolumeRoots = []string{
	"/",
	"/proc",
	"/sys",
	"/etc",
	"/dev",
	"/boot",
	"/root",
	"/run",     // docker.sock, podman.sock, dbus, sshd agent sockets, …
	"/var/run", // symlink to /run on modern systems; listed for hosts where EvalSymlinks can't resolve it
	"/var/lib/docker",
	"/var/lib/containers",
}

// namedVolumeRe matches a docker/podman named volume (as opposed to a host
// path bind mount). Named volumes are runtime-managed and safe.
var namedVolumeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Validate checks the task's container host configuration against the
// policy and returns an error describing every violation when the config
// requests a dangerous escape that the operator has not allowed. A nil
// DockerConfig validates trivially. Both the docker and podman runtimes
// call this before creating a container; a non-nil error aborts the run.
func Validate(cfg *task.DockerConfig, p Policy) error {
	if cfg == nil {
		return nil
	}
	var violations []string

	if v := checkNetworkMode(cfg.NetworkMode, p); v != "" {
		violations = append(violations, v)
	}
	violations = append(violations, checkCapAdd(cfg.CapAdd, p)...)
	violations = append(violations, checkSecurityOpt(cfg.SecurityOpt, p)...)
	violations = append(violations, checkVolumes(cfg.Volumes, p)...)

	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("container security policy: %s — dangerous host config is denied by default; an operator can opt in via the container_security block in dicode.yaml",
		strings.Join(violations, "; "))
}

func checkNetworkMode(mode string, p Policy) string {
	m := strings.ToLower(strings.TrimSpace(mode))
	dangerous := m == "host" || strings.HasPrefix(m, "container:") || strings.HasPrefix(m, "ns:")
	if dangerous && !p.AllowHostNetwork {
		return fmt.Sprintf("docker.network_mode %q joins a host/container namespace (allow with container_security.allow_host_network)", mode)
	}
	return ""
}

// NormalizeCap upper-cases a capability name and strips an optional CAP_
// prefix so CAP_SYS_ADMIN, sys_admin and SYS_ADMIN compare equal.
func NormalizeCap(c string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")
}

func checkCapAdd(caps []string, p Policy) []string {
	allowed := make(map[string]bool, len(p.AllowedCapAdd))
	for _, a := range p.AllowedCapAdd {
		allowed[NormalizeCap(a)] = true
	}
	var violations []string
	for _, c := range caps {
		n := NormalizeCap(c)
		if !dangerousCapAdd[n] {
			continue
		}
		if allowed[n] || allowed["ALL"] {
			continue
		}
		violations = append(violations, fmt.Sprintf("docker.cap_add %q can enable container escape (allow with container_security.allowed_cap_add)", c))
	}
	return violations
}

func checkSecurityOpt(opts []string, p Policy) []string {
	var violations []string
	for _, o := range opts {
		// Canonicalize: lower-case, strip all internal whitespace (so
		// "seccomp = unconfined" can't slip past the exact match), then
		// normalize the first ":" to "=" (Docker historically accepted ":"
		// as the key/value separator) so seccomp:unconfined is caught.
		canon := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(o)), " ", "")
		canon = strings.Replace(canon, ":", "=", 1)
		dangerous := canon == "seccomp=unconfined" ||
			canon == "apparmor=unconfined" ||
			canon == "label=disable" ||
			canon == "systempaths=unconfined" ||
			strings.HasPrefix(canon, "unmask=")
		if dangerous && !p.AllowInsecureSecurityOpt {
			violations = append(violations, fmt.Sprintf("docker.security_opt %q disables a kernel sandbox layer (allow with container_security.allow_insecure_security_opt)", o))
		}
	}
	return violations
}

func checkVolumes(volumes []string, p Policy) []string {
	var violations []string
	for _, v := range volumes {
		if msg := checkVolume(v, p); msg != "" {
			violations = append(violations, msg)
		}
	}
	return violations
}

// checkVolume validates a single "source:container[:opts]" volume spec.
// Returns "" when the spec is allowed, otherwise a violation message.
func checkVolume(spec string, p Policy) string {
	parts := strings.Split(spec, ":")
	src := strings.TrimSpace(parts[0])
	if len(parts) == 1 || src == "" {
		// Anonymous volume ("/data") or malformed empty source — the
		// runtime manages storage itself; no host path is exposed.
		return ""
	}
	if !strings.HasPrefix(src, "/") {
		if namedVolumeRe.MatchString(src) {
			return "" // named volume, runtime-managed
		}
		// Relative path (./x, ../x, ~/x). Podman bind-mounts these relative
		// to the daemon CWD — reject so traversal can't sidestep the
		// absolute-path checks below.
		return fmt.Sprintf("docker.volumes source %q is a relative host path; bind-mount sources must be absolute", src)
	}

	resolved := resolveHostPath(src)

	if len(p.AllowedVolumeRoots) > 0 {
		for _, root := range p.AllowedVolumeRoots {
			if pathWithin(resolved, filepath.Clean(root)) {
				return ""
			}
		}
		return fmt.Sprintf("docker.volumes source %q (resolved %q) is outside the configured container_security.allowed_volume_roots", src, resolved)
	}

	for _, root := range sensitiveVolumeRoots {
		// "/" only blocks bind-mounting the root itself; everything else
		// blocks the path and its whole subtree.
		if root == "/" {
			if resolved == "/" {
				return fmt.Sprintf("docker.volumes source %q bind-mounts the host root filesystem (allow specific paths with container_security.allowed_volume_roots)", src)
			}
			continue
		}
		if pathWithin(resolved, root) {
			return fmt.Sprintf("docker.volumes source %q resolves to sensitive host path %q (allow explicitly with container_security.allowed_volume_roots)", src, resolved)
		}
	}
	// Control sockets relocated outside the standard dirs (e.g. a custom
	// DOCKER_HOST socket path) are still recognizable by name.
	switch filepath.Base(resolved) {
	case "docker.sock", "podman.sock":
		return fmt.Sprintf("docker.volumes source %q resolves to a container-runtime control socket (%q)", src, resolved)
	}
	return ""
}

// resolveHostPath cleans ".." segments and resolves symlinks best-effort so
// a source like /home/user/../../etc or a symlink pointing at /proc cannot
// dodge the sensitive-path checks. Paths that do not exist on the host fall
// back to the lexically cleaned form.
func resolveHostPath(src string) string {
	cleaned := filepath.Clean(src)
	if !filepath.IsAbs(cleaned) {
		if abs, err := filepath.Abs(cleaned); err == nil {
			cleaned = abs
		}
	}
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	return cleaned
}

// pathWithin reports whether path equals root or lives inside root's subtree.
// Both arguments must already be cleaned absolute paths.
func pathWithin(path, root string) bool {
	if root == "/" {
		return true
	}
	return path == root || strings.HasPrefix(path, root+"/")
}
