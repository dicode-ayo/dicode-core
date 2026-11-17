package gitops

import (
	"net/url"
	"path/filepath"
)

// TestFixtureRemoteURL returns a file:// URL for the local bare repo at
// bareDir, for tests that clone a fixture through CloneAtRef.
//
// ValidateRemoteHost rejects host-less remotes, so the URL carries the
// reserved placeholder host test-fixture.invalid; go-git's file transport
// reads only the path. Production config never admits file:// remotes
// (pkg/taskset.ValidateRefURL allows http/https/ssh only).
//
// bareDir must be absolute on the host OS, or a drive path (C:/... or C:\...,
// recognised on any OS), which becomes file://test-fixture.invalid/C:/...
// (go-git drops the slash before the drive letter). Anything else panics.
func TestFixtureRemoteURL(bareDir string) string {
	p := filepath.ToSlash(bareDir)
	if isDrivePath(p) {
		p = "/" + p
	} else if !filepath.IsAbs(bareDir) {
		panic("gitops.TestFixtureRemoteURL: bareDir must be absolute, got " + bareDir)
	}
	return (&url.URL{Scheme: "file", Host: "test-fixture.invalid", Path: p}).String()
}

// isDrivePath reports whether p starts with a drive letter, a colon and a
// slash, independent of the host OS.
func isDrivePath(p string) bool {
	return len(p) >= 3 && p[1] == ':' && p[2] == '/' &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}
