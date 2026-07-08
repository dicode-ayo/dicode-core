package gitops

import (
	"fmt"
	"net"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// ValidateRemoteHost rejects remote git URLs that point at loopback,
// private, link-local, or otherwise internal network targets (SSRF guard,
// #475/#481/#489). It inspects the literal host only: IP literals are
// classified with IsBlockedIP; hostnames are matched against well-known
// internal suffixes (localhost, *.local mDNS, *.internal cloud-metadata
// style names). DNS resolution is deliberately not performed here — that
// is the job of the separate, dial-time layer (guardedDialContext /
// InstallSSRFGuardedTransport in dialguard.go), which only covers the
// http/https schemes.
//
// This function is scheme-agnostic: it parses rawURL via go-git's own
// transport.NewEndpoint, which understands http(s), ssh, git, and the SCP
// shorthand form (git@host:path) uniformly. That makes this the ONLY SSRF
// guard ssh:// and SCP-shorthand remotes get — the dial-time transport
// guard is installed solely for http/https clients, since go-git's ssh/git
// transports have no shared, injectable dial choke point (see
// InstallSSRFGuardedTransport's doc comment).
//
// Call this from every entry point that accepts a caller/config-supplied
// git remote URL before any network operation happens — both the
// branch-preview helper (pkg/source/git.ListBranches) and the actual
// clone/pull path (CloneOrPull) call this same function, so there is
// exactly one place that can drift out of sync (#489 found CloneOrPull
// skipping the check entirely while ListBranches had it).
func ValidateRemoteHost(rawURL string) error {
	ep, err := transport.NewEndpoint(rawURL)
	if err != nil {
		return fmt.Errorf("invalid remote url: %w", err)
	}

	// go-git stores IPv6 literals bracketed ("[::1]"); strip for parsing.
	host := strings.ToLower(strings.Trim(ep.Host, "[]"))
	if host == "" {
		return fmt.Errorf("url has no remote host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return fmt.Errorf("host %q is a private or internal address; refusing to contact it", host)
		}
		return nil
	}
	if host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("host %q is a private or internal address; refusing to contact it", host)
	}
	return nil
}
