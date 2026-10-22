package python

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dicode/dicode/pkg/runtime/perms/permstest"
	"github.com/dicode/dicode/pkg/task"
)

// A "../" write grant reaches the audit hook already cleaned, matching what
// the Deno renderer emits for the same declaration.
func TestBuildGuardPolicy_FSWriteCleaned(t *testing.T) {
	spec := specWithPerms(task.Permissions{FS: []task.FSEntry{
		{Path: "../../etc", Permission: "w"},
		{Path: "a//b/../c/", Permission: "rw"},
	}})
	pol := buildGuardPolicy(spec, "/tmp/dicode.sock", nil)
	want := []string{
		"/tmp/dicode.sock",
		"/srv/etc",
		filepath.Join(spec.TaskDir, "a", "c"),
	}
	if !reflect.DeepEqual(pol.FSWrite, want) {
		t.Errorf("fs_write = %v, want %v", pol.FSWrite, want)
	}
}

func TestRenderersAgree_Python(t *testing.T) {
	for _, c := range permstest.Cases {
		t.Run(c.Name, func(t *testing.T) {
			pol := buildGuardPolicy(c.Spec(), "/tmp/dicode.sock", nil)
			wantNet := "deny"
			switch {
			case c.NetUnrestricted:
				wantNet = "unrestricted"
			case len(c.NetHosts) > 0:
				wantNet = "allowlist"
			}
			wantRun := "deny"
			switch {
			case c.RunAllow:
				wantRun = "allow"
			case len(c.RunCommands) > 0:
				wantRun = "allowlist"
			}
			if pol.Net.Mode != wantNet || !reflect.DeepEqual(pol.Net.Hosts, c.NetHosts) {
				t.Errorf("net = %+v, want %s %v", pol.Net, wantNet, c.NetHosts)
			}
			if pol.Run.Mode != wantRun || !reflect.DeepEqual(pol.Run.Commands, c.RunCommands) {
				t.Errorf("run = %+v, want %s %v", pol.Run, wantRun, c.RunCommands)
			}
			want := append([]string{"/tmp/dicode.sock"}, c.WritePaths...)
			if !reflect.DeepEqual(pol.FSWrite, want) {
				t.Errorf("fs_write = %v, want %v", pol.FSWrite, want)
			}
		})
	}
}
