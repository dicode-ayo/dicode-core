package gitops

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

var drivePath = regexp.MustCompile(`^[A-Za-z]:/`)

// TestFixtureRemoteURL returns a file:// URL for the local bare repo at
// bareDir, for tests that clone a fixture through CloneAtRef.
//
// ValidateRemoteHost rejects host-less remotes, so the URL carries the
// reserved placeholder host test-fixture.invalid; go-git's file transport
// reads only the path. Production config never admits file:// remotes
// (pkg/taskset.ValidateRefURL allows http/https/ssh only).
//
// bareDir must be absolute: a POSIX path, or a Windows drive path, which
// becomes file://test-fixture.invalid/C:/... (go-git drops the slash before
// the drive letter). Anything else panics.
func TestFixtureRemoteURL(bareDir string) string {
	p := filepath.ToSlash(bareDir)
	switch {
	case strings.HasPrefix(p, "/"):
	case drivePath.MatchString(p):
		p = "/" + p
	default:
		panic("gitops.TestFixtureRemoteURL: bareDir must be absolute, got " + bareDir)
	}
	return (&url.URL{Scheme: "file", Host: "test-fixture.invalid", Path: p}).String()
}
