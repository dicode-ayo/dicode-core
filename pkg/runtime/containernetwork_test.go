package runtime_test

import (
	"testing"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

func TestEffectiveNetworkMode(t *testing.T) {
	tests := []struct {
		name         string
		declaredMode string
		netPerms     []string
		ports        []string
		want         string
	}{
		// Zero-default: deny all network when nothing is declared.
		{name: "no mode, no perms, no ports → none", declaredMode: "", netPerms: nil, ports: nil, want: "none"},
		{name: "no mode, empty perms, no ports → none", declaredMode: "", netPerms: []string{}, ports: nil, want: "none"},
		// Port publishing requires a network interface — never default to none.
		{name: "no mode, no perms, has ports → bridge default", declaredMode: "", netPerms: nil, ports: []string{"8888:80"}, want: ""},
		{name: "no mode, empty perms, has ports → bridge default", declaredMode: "", netPerms: []string{}, ports: []string{"443:443"}, want: ""},
		// Explicit network permissions grant connectivity.
		{name: "no mode, wildcard perms, no ports → bridge default", declaredMode: "", netPerms: []string{"*"}, ports: nil, want: ""},
		{name: "no mode, specific host, no ports → bridge default", declaredMode: "", netPerms: []string{"api.github.com"}, ports: nil, want: ""},
		{name: "no mode, multiple hosts, no ports → bridge default", declaredMode: "", netPerms: []string{"api.github.com", "hooks.slack.com"}, ports: nil, want: ""},
		// Explicit docker.network_mode always wins.
		{name: "explicit bridge → bridge", declaredMode: "bridge", netPerms: nil, ports: nil, want: "bridge"},
		{name: "explicit none → none (even with perms)", declaredMode: "none", netPerms: []string{"*"}, ports: []string{"80:80"}, want: "none"},
		{name: "explicit host → host (even with perms)", declaredMode: "host", netPerms: nil, ports: nil, want: "host"},
		{name: "explicit bridge with perms → bridge", declaredMode: "bridge", netPerms: []string{"api.github.com"}, ports: nil, want: "bridge"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perms := task.Permissions{Net: tt.netPerms}
			got := pkgruntime.EffectiveNetworkMode(tt.declaredMode, perms, tt.ports)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNetPermsNeedWarning(t *testing.T) {
	tests := []struct {
		name         string
		declaredMode string
		netPerms     []string
		want         bool
	}{
		// No warning needed for clean cases.
		{name: "no mode, no perms → no warning (will be none)", declaredMode: "", netPerms: nil, want: false},
		{name: "no mode, empty perms → no warning (will be none)", declaredMode: "", netPerms: []string{}, want: false},
		{name: "no mode, wildcard → no warning (explicit unrestricted)", declaredMode: "", netPerms: []string{"*"}, want: false},
		// Warning: specific hosts declared but can't be enforced per-host.
		{name: "no mode, one host → warning", declaredMode: "", netPerms: []string{"api.github.com"}, want: true},
		{name: "no mode, multiple hosts → warning", declaredMode: "", netPerms: []string{"api.github.com", "auth.example.com"}, want: true},
		// Explicit mode suppresses warning — the operator made an intentional choice.
		{name: "explicit bridge, specific host → no warning", declaredMode: "bridge", netPerms: []string{"api.github.com"}, want: false},
		{name: "explicit none, specific host → no warning", declaredMode: "none", netPerms: []string{"api.github.com"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perms := task.Permissions{Net: tt.netPerms}
			got := pkgruntime.NetPermsNeedWarning(tt.declaredMode, perms)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
