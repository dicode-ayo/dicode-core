package gitops

// TestFixtureRemoteURL returns a file:// URL for bareDir (an absolute path to
// a local bare git repo) suitable for use as a git remote in tests that need
// to exercise the real CloneOrPull/ValidateRemoteHost code path against a
// local fixture repo instead of a network remote.
//
// file:// URLs are host-less by construction — go-git's file transport only
// ever reads Endpoint.Path, never Endpoint.Host — but ValidateRemoteHost's
// SSRF guard (wired into CloneOrPull for #489) rejects any host-less remote
// as indistinguishable from an unparseable/attacker-supplied URL missing a
// host entirely. Prepending a placeholder, obviously-fake hostname (RFC 2606
// reserves the .invalid TLD for exactly this) satisfies the guard without
// weakening it: no production caller can ever reach CloneOrPull with a
// file:// URL in the first place (pkg/taskset.ValidateRefURL only allows
// http/https/ssh at config-load time), so this is purely a test-fixture
// convenience, not a bypass.
//
// Shared by pkg/source/git, pkg/taskset, and pkg/webui test fixtures so the
// hostname choice and its rationale live in exactly one place.
func TestFixtureRemoteURL(bareDir string) string {
	return "file://test-fixture.invalid" + bareDir
}
