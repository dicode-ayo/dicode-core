package runtime

import "github.com/dicode/dicode/pkg/task"

// EffectiveNetworkMode derives the container network mode for a Docker/Podman task.
//
// Zero-default: when no explicit docker.network_mode is declared, the task has no
// network permissions (permissions.net empty), and publishes no ports, the returned
// mode is "none" — enforcing the zero-default-permissions policy for outbound network.
//
// Tasks that need outbound network must declare permissions.net (any non-empty list).
// Tasks that publish ports (docker.ports) implicitly need a network interface and are
// never defaulted to "none" regardless of permissions.net.
//
// An explicit docker.network_mode always takes precedence over the derived default.
func EffectiveNetworkMode(declaredMode string, perms task.Permissions, ports []string) string {
	if declaredMode != "" {
		return declaredMode
	}
	if len(perms.Net) == 0 && len(ports) == 0 {
		return "none"
	}
	// Has network permissions or publishes ports: let the runtime pick its default.
	return ""
}

// NetPermsNeedWarning reports whether the task's network permissions warrant a
// warning because per-host enforcement is not yet available for Docker/Podman.
//
// Returns true only when: no explicit docker.network_mode is set AND permissions.net
// lists specific hosts (neither empty nor the ["*"] wildcard). In that case the
// container gets unrestricted network access even though the task intends to restrict
// it to named hosts — the host list is informational only for Docker/Podman today.
func NetPermsNeedWarning(declaredMode string, perms task.Permissions) bool {
	if declaredMode != "" {
		return false
	}
	n := perms.Net
	if len(n) == 0 {
		return false // no permissions → "none" mode, no mismatch
	}
	if len(n) == 1 && n[0] == "*" {
		return false // explicit wildcard → unrestricted by design
	}
	return true // specific hosts declared, can't enforce per-host for containers
}
