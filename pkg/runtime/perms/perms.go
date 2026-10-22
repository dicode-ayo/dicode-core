// Package perms resolves a task's declared permissions once into a
// runtime-neutral EffectivePermissions, which each runtime then renders into
// its own enforcement form (Deno --allow-* flags, the Python audit-hook
// policy). Parsing the "*" wildcard, expanding "~" and canonicalising paths
// live here and nowhere else.
//
// Script-visible env: the Deno sandbox grants only DICODE_SOCKET, DICODE_TOKEN,
// HOME, DENO_DIR and XDG_CACHE_HOME plus the declared names, while the Python
// guard's allowlist is the larger runtime-essential set (PATH, locale, proxies,
// TLS, uv locations, ...) because the interpreter reads those itself. Each
// renderer owns its baseline; EffectivePermissions carries only the declared
// names.
package perms

import (
	"os"
	"path/filepath"
	"strings"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

// NetMode is the normalised outbound-network policy.
type NetMode int

const (
	// NetDeny is the default: omitted or empty permissions.net.
	NetDeny NetMode = iota
	// NetAllowlist permits only the hosts in EffectivePermissions.NetHosts.
	NetAllowlist
	// NetUnrestricted is permissions.net: ["*"].
	NetUnrestricted
)

// RunMode is the normalised subprocess-execution policy.
type RunMode int

const (
	// RunDeny is the default: omitted or empty permissions.run.
	RunDeny RunMode = iota
	// RunAllowlist permits only the commands in EffectivePermissions.RunCommands.
	RunAllowlist
	// RunAllow is permissions.run: ["*"].
	RunAllow
)

// EffectivePermissions is the resolved form of task.Permissions.
type EffectivePermissions struct {
	NetMode  NetMode
	NetHosts []string // set only for NetAllowlist

	RunMode     RunMode
	RunCommands []string // set only for RunAllowlist

	// SysAll is permissions.sys: ["*"]; otherwise SysNames is the allowlist.
	SysAll   bool
	SysNames []string

	// ReadPaths and WritePaths are the declared fs grants, home-expanded,
	// resolved against the task dir and cleaned. "rw" appears in both.
	ReadPaths  []string
	WritePaths []string

	// EnvReadExposed means any env var may be read.
	EnvReadExposed bool
	// EnvNames are the declared readable names followed by the host vars
	// matched by pattern entries. It may contain duplicates across the two.
	EnvNames []string
}

// Resolve derives the effective permissions of spec.
func Resolve(spec *task.Spec) EffectivePermissions {
	p := spec.Permissions
	var e EffectivePermissions

	switch {
	case isWildcard(p.Net):
		e.NetMode = NetUnrestricted
	case len(p.Net) > 0:
		e.NetMode = NetAllowlist
		e.NetHosts = append([]string(nil), p.Net...)
	}

	switch {
	case isWildcard(p.Run):
		e.RunMode = RunAllow
	case len(p.Run) > 0:
		e.RunMode = RunAllowlist
		e.RunCommands = append([]string(nil), p.Run...)
	}

	if isWildcard(p.Sys) {
		e.SysAll = true
	} else {
		e.SysNames = append([]string(nil), p.Sys...)
	}

	for _, entry := range p.FS {
		path := ExpandHome(entry.Path)
		if !filepath.IsAbs(path) {
			path = filepath.Join(spec.TaskDir, path)
		}
		path = filepath.Clean(path)
		switch entry.Permission {
		case "r":
			e.ReadPaths = append(e.ReadPaths, path)
		case "w":
			e.WritePaths = append(e.WritePaths, path)
		case "rw":
			e.ReadPaths = append(e.ReadPaths, path)
			e.WritePaths = append(e.WritePaths, path)
		}
	}

	e.EnvReadExposed = p.EnvReadExposed
	for _, entry := range p.Env {
		// A pattern entry's literal name ("GITHUB_*") is not a readable var;
		// its host matches are appended below.
		if entry.Name == "" || pkgruntime.IsWildcardEnvEntry(entry) {
			continue
		}
		e.EnvNames = append(e.EnvNames, entry.Name)
	}
	if !e.EnvReadExposed {
		e.EnvNames = append(e.EnvNames, pkgruntime.WildcardEnvNames(spec)...)
	}
	return e
}

// ExpandHome replaces a leading "~" or "~/" with the user home directory.
func ExpandHome(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + p[1:]
		}
	}
	return p
}

func isWildcard(list []string) bool {
	return len(list) == 1 && list[0] == "*"
}
