package gitops

import (
	"context"
	"errors"
	"github.com/go-git/go-git/v5/plumbing"
	"os"
	"strings"
	"testing"
)

// Tests for ValidateRemoteHost, the shared SSRF literal-host guard
// (#475/#481/#489). This is now the ONLY guard ssh:// and SCP-shorthand
// remotes get (the dial-time layer in dialguard.go only covers http/https),
// so it must reject blocked hosts uniformly across every scheme go-git's
// transport.NewEndpoint understands.

type hostGuardCase struct {
	name    string
	url     string
	blocked bool
}

func TestValidateRemoteHost(t *testing.T) {
	blockedHosts := []string{
		"127.0.0.1",
		"10.1.2.3",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"[::1]",
		"[fe80::1]",
		"[fd00::1]",
		"0.0.0.0",
	}

	var cases []hostGuardCase

	// IP-literal blocked hosts across http/https/ssh.
	for _, h := range blockedHosts {
		for _, scheme := range []string{"http", "https", "ssh"} {
			cases = append(cases, hostGuardCase{
				name:    scheme + "://" + h,
				url:     scheme + "://" + h + "/repo.git",
				blocked: true,
			})
		}
	}

	// Hostname-suffix blocked cases.
	for _, h := range []string{"localhost", "foo.localhost", "gitserver.local", "metadata.google.internal", "foo.corp.internal"} {
		cases = append(cases, hostGuardCase{
			name:    "https://" + h,
			url:     "https://" + h + "/repo.git",
			blocked: true,
		})
	}

	// SCP-shorthand cases: blocked host and blocked-suffix host.
	cases = append(cases,
		hostGuardCase{name: "scp-shorthand loopback", url: "git@127.0.0.1:org/repo.git", blocked: true},
		hostGuardCase{name: "scp-shorthand internal suffix", url: "git@internal.corp.internal:org/repo.git", blocked: true},
	)

	// Allowed/public cases across schemes plus SCP-shorthand.
	cases = append(cases,
		hostGuardCase{name: "https public", url: "https://github.com/org/repo.git", blocked: false},
		hostGuardCase{name: "http public", url: "http://gitlab.example.com/group/repo.git", blocked: false},
		hostGuardCase{name: "ssh public", url: "ssh://git@github.com/org/repo.git", blocked: false},
		hostGuardCase{name: "scp-shorthand public", url: "git@github.com:org/repo.git", blocked: false},
		hostGuardCase{name: "public IP literal", url: "https://8.8.8.8/repo.git", blocked: false},
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRemoteHost(tc.url)
			if tc.blocked {
				if err == nil {
					t.Fatalf("ValidateRemoteHost(%q) = nil, want private/internal rejection", tc.url)
				}
				if !strings.Contains(err.Error(), "private or internal") {
					t.Fatalf("ValidateRemoteHost(%q) error = %q, want private/internal rejection", tc.url, err)
				}
				if !errors.Is(err, ErrBlockedHost) {
					t.Fatalf("ValidateRemoteHost(%q) error = %v, want errors.Is(ErrBlockedHost)", tc.url, err)
				}
			} else if err != nil {
				t.Fatalf("ValidateRemoteHost(%q) = %v, want nil", tc.url, err)
			}
		})
	}
}

// TestValidateRemoteHost_UnparseableURL proves a malformed URL that go-git's
// transport.NewEndpoint cannot parse returns a non-nil, wrapped error rather
// than panicking or silently passing.
func TestValidateRemoteHost_UnparseableURL(t *testing.T) {
	// An unclosed IPv6-literal bracket is rejected by net/url.Parse (which
	// transport.NewEndpoint delegates to for scheme-prefixed URLs) — unlike
	// most malformed strings, which NewEndpoint's SCP-like/file-path
	// fallbacks happily absorb instead of erroring.
	url := "http://[::1"
	err := ValidateRemoteHost(url)
	if err == nil {
		t.Fatalf("ValidateRemoteHost(%q) = nil, want parse error", url)
	}
	if !strings.Contains(err.Error(), "invalid remote url") {
		t.Errorf("ValidateRemoteHost error = %q, want wrapped \"invalid remote url\"", err)
	}
	if !errors.Is(err, ErrNoRemoteHost) {
		t.Errorf("ValidateRemoteHost error = %v, want errors.Is(ErrNoRemoteHost)", err)
	}
}

// TestValidateRemoteHost_RejectsMissingHost mirrors pkg/source/git's
// TestValidateRemoteHost_RejectsMissingHost: a file:// endpoint carries no
// remote host at all and must be rejected with a distinct "no remote host"
// error, not the private/internal message.
func TestValidateRemoteHost_RejectsMissingHost(t *testing.T) {
	err := ValidateRemoteHost("file:///etc/passwd")
	if err == nil {
		t.Fatal("ValidateRemoteHost(file:///etc/passwd) = nil, want no-host rejection")
	}
	if !strings.Contains(err.Error(), "no remote host") {
		t.Errorf("ValidateRemoteHost(file:///etc/passwd) error = %q, want \"no remote host\"", err)
	}
	if !errors.Is(err, ErrNoRemoteHost) {
		t.Errorf("ValidateRemoteHost(file:///etc/passwd) error = %v, want errors.Is(ErrNoRemoteHost)", err)
	}
}

// TestCloneAtRef_RejectsMaliciousSSHRemote is the core regression test for
// #489: before this fix, an ssh:// or SCP-shorthand remote reached
// CloneAtRef with zero host validation at any layer (the dial-time guard in
// dialguard.go only covers http/https, and the literal-host guard was only
// ever wired into ListBranches). This proves CloneAtRef rejects such a
// URL before any clone is attempted — the target directory must never be
// populated with a .git subdirectory.
func TestCloneAtRef_RejectsMaliciousSSHRemote(t *testing.T) {
	cases := []string{
		"git@127.0.0.1:org/repo.git",
		"ssh://git@127.0.0.1/org/repo.git",
	}
	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			tmpDir := t.TempDir()
			err := CloneAtRef(context.Background(), tmpDir, url, plumbing.NewBranchReferenceName("main"), nil)
			if err == nil {
				t.Fatalf("CloneAtRef(%q) = nil, want private/internal rejection", url)
			}
			if !strings.Contains(err.Error(), "private or internal address") {
				t.Fatalf("CloneAtRef(%q) error = %q, want private/internal rejection", url, err)
			}
			if _, statErr := os.Stat(tmpDir + "/.git"); statErr == nil {
				t.Fatalf("CloneAtRef(%q) populated %s/.git — a clone was attempted", url, tmpDir)
			}
		})
	}
}

// TestValidateRemoteHost_RejectsIPv6ZoneID is the regression test for the
// zone-ID bypass: net.ParseIP returns nil for a zone-scoped IPv6 literal
// like "fe80::1%eth0" (RFC 4007), so before this fix such a host fell
// through the IP-literal branch, matched none of the hostname-suffix
// checks, and was silently *allowed* — a real SSRF bypass for ssh://
// remotes specifically, since they have no dial-time recheck to catch it
// after the fact (see dialguard.go's doc comment: that layer only covers
// http/https).
func TestValidateRemoteHost_RejectsIPv6ZoneID(t *testing.T) {
	cases := []string{
		// %25 is the correctly-percent-encoded form of the zone-ID
		// delimiter '%' inside a URI (RFC 6874); go-git's endpoint parser
		// decodes it back to a literal '%' in ep.Host.
		"ssh://git@[fe80::1%25eth0]/repo.git",
		"http://[fe80::1%25eth0]/repo.git",
		"https://[fe80::1%25eth0]/repo.git",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			err := ValidateRemoteHost(u)
			if err == nil {
				t.Fatalf("ValidateRemoteHost(%q) = nil, want zone-ID rejection", u)
			}
			if !strings.Contains(err.Error(), "private or internal") {
				t.Errorf("ValidateRemoteHost(%q) error = %q, want private/internal rejection", u, err)
			}
			if !errors.Is(err, ErrBlockedHost) {
				t.Errorf("ValidateRemoteHost(%q) error = %v, want errors.Is(ErrBlockedHost)", u, err)
			}
		})
	}
}

// TestValidateRemoteHost_RejectsTrailingDotSuffixBypass is the regression
// test for fix #4 (defense-in-depth): a trailing dot is a valid FQDN-root
// marker DNS resolvers strip before lookup, so
// "metadata.google.internal." resolves identically to
// "metadata.google.internal" — but strings.HasSuffix(host, ".internal")
// would not match the trailing-dot form (it ends in "internal.", not
// ".internal"), letting it slip past the hostname-suffix guard entirely.
func TestValidateRemoteHost_RejectsTrailingDotSuffixBypass(t *testing.T) {
	cases := []string{
		"https://metadata.google.internal./computeMetadata/v1",
		"https://gitserver.local./repo.git",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			err := ValidateRemoteHost(u)
			if err == nil {
				t.Fatalf("ValidateRemoteHost(%q) = nil, want trailing-dot suffix rejection", u)
			}
			if !strings.Contains(err.Error(), "private or internal") {
				t.Errorf("ValidateRemoteHost(%q) error = %q, want private/internal rejection", u, err)
			}
		})
	}
}

// TestCloneAtRef_PublicHostPassesHostGuard proves CloneAtRef's
// ValidateRemoteHost call does not reject a legitimate public host — the
// SSRF guard must not be a false-positive tax on normal operation. Rather
// than driving a real (slow, non-hermetic, likely network-less-in-CI) clone
// attempt against github.com, this asserts the same host-guard function
// CloneAtRef calls passes for a public URL, which is what actually
// determines whether CloneAtRef would proceed past the guard to the real
// clone/dial stage.
func TestCloneAtRef_PublicHostPassesHostGuard(t *testing.T) {
	url := "https://github.com/example/repo.git"
	if err := ValidateRemoteHost(url); err != nil {
		t.Fatalf("ValidateRemoteHost(%q) = %v, want nil (CloneAtRef must not reject public hosts)", url, err)
	}
}
