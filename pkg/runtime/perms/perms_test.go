package perms

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func spec(p task.Permissions) *task.Spec {
	return &task.Spec{ID: "t/p", TaskDir: "/srv/tasks/p", Permissions: p}
}

func TestResolve_NetRunSysModes(t *testing.T) {
	cases := []struct {
		name  string
		list  []string
		net   NetMode
		run   RunMode
		sysAl bool
	}{
		{"nil", nil, NetDeny, RunDeny, false},
		{"empty", []string{}, NetDeny, RunDeny, false},
		{"wildcard", []string{"*"}, NetUnrestricted, RunAllow, true},
		{"named", []string{"a.example", "b.example"}, NetAllowlist, RunAllowlist, false},
		{"wildcard among others is a plain entry", []string{"*", "a"}, NetAllowlist, RunAllowlist, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := Resolve(spec(task.Permissions{Net: c.list, Run: c.list, Sys: c.list}))
			if e.NetMode != c.net || e.RunMode != c.run || e.SysAll != c.sysAl {
				t.Errorf("got net=%v run=%v sysAll=%v, want %v %v %v", e.NetMode, e.RunMode, e.SysAll, c.net, c.run, c.sysAl)
			}
			if c.net == NetAllowlist && !reflect.DeepEqual(e.NetHosts, c.list) {
				t.Errorf("NetHosts = %v", e.NetHosts)
			}
			if c.net != NetAllowlist && e.NetHosts != nil {
				t.Errorf("NetHosts = %v, want nil", e.NetHosts)
			}
		})
	}
}

func TestResolve_FSCanonical(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	e := Resolve(spec(task.Permissions{FS: []task.FSEntry{
		{Path: "../../etc", Permission: "w"},
		{Path: "data//x/", Permission: "rw"},
		{Path: "~/cfg/../cache", Permission: "r"},
		{Path: "/abs/./p", Permission: "r"},
		{Path: "ignored", Permission: "x"},
	}}))
	wantR := []string{"/srv/tasks/p/data/x", filepath.Join(home, "cache"), "/abs/p"}
	wantW := []string{"/srv/etc", "/srv/tasks/p/data/x"}
	if !reflect.DeepEqual(e.ReadPaths, wantR) {
		t.Errorf("ReadPaths = %v, want %v", e.ReadPaths, wantR)
	}
	if !reflect.DeepEqual(e.WritePaths, wantW) {
		t.Errorf("WritePaths = %v, want %v", e.WritePaths, wantW)
	}
}

func TestResolve_Env(t *testing.T) {
	t.Setenv("PERMSTEST_A", "1")
	e := Resolve(spec(task.Permissions{
		Env: []task.EnvEntry{{Name: "FOO"}, {Name: "PERMSTEST_*"}, {Name: "BAR", Value: "v"}},
	}))
	want := []string{"FOO", "BAR", "PERMSTEST_A"}
	if !reflect.DeepEqual(e.EnvNames, want) {
		t.Errorf("EnvNames = %v, want %v", e.EnvNames, want)
	}
	if e.EnvReadExposed {
		t.Error("EnvReadExposed set")
	}
	if !Resolve(spec(task.Permissions{EnvReadExposed: true})).EnvReadExposed {
		t.Error("EnvReadExposed not carried")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	for in, want := range map[string]string{
		"~":       home,
		"~/x":     home + "/x",
		"~x":      "~x",
		"/a/~/b":  "/a/~/b",
		"rel/dir": "rel/dir",
	} {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
