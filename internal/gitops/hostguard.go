package gitops

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// ErrBlockedHost is the sentinel wrapped into every error ValidateRemoteHost
// returns for a host it classifies as loopback/private/link-local/internal
// (or a format it treats as blocked outright, like an IPv6 zone-ID literal).
// Callers use errors.Is(err, ErrBlockedHost) to recognise the rejection as a
// permanent, non-retryable configuration error rather than a transient
// network failure — see pkg/source/git's isPermanentGitError, which classifies
// the retry loop this way so a blocked host doesn't burn the full
// ~30s retry budget on every poll tick (#489 follow-up).
var ErrBlockedHost = errors.New("blocked host")

// ErrNoRemoteHost is the sentinel wrapped into every error ValidateRemoteHost
// returns when rawURL cannot be parsed into a remote endpoint at all, or
// parses but carries no host component (e.g. a bare file:// path). Also
// treated as permanent/non-retryable by isPermanentGitError: a URL that will
// never have a host doesn't get one on the next poll tick.
var ErrNoRemoteHost = errors.New("no remote host")

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
// clone/refresh path (CloneAtRef) call this same function, so there is
// exactly one place that can drift out of sync (#489 found the clone path
// skipping the check entirely while ListBranches had it).
func ValidateRemoteHost(rawURL string) error {
	ep, err := transport.NewEndpoint(rawURL)
	if err != nil {
		return fmt.Errorf("invalid remote url: %w: %w", err, ErrNoRemoteHost)
	}

	// normalizeHost strips go-git's IPv6 brackets and lowercases. It also drops
	// a trailing FQDN-root dot: "metadata.google.internal." resolves identically
	// to "metadata.google.internal" but would otherwise slip past the literal
	// hostname-suffix checks below (which match ".internal"/".local" as a true
	// suffix). Sharing this with the allowlist matcher keeps both sides comparing
	// under one canonical form.
	host := normalizeHost(ep.Host)
	if host == "" {
		return fmt.Errorf("url has no remote host: %w", ErrNoRemoteHost)
	}
	// A zone-ID suffix (RFC 4007, e.g. "fe80::1%eth0") makes net.ParseIP
	// return nil even though the host is a link-local IPv6 literal — without
	// this check it would fall through every branch below and be silently
	// *allowed*. Reject outright as blocked rather than attempting to parse
	// the zone: this is the ONLY guard ssh:// and SCP-shorthand remotes get
	// (see the package doc comment above), so failing open here would be a
	// real, exploitable SSRF bypass, not just a false negative on an obscure
	// input.
	if strings.Contains(host, "%") {
		return fmt.Errorf("host %q is a private or internal address; refusing to contact it (permit it via source_security.allow_internal_hosts): %w", host, ErrBlockedHost)
	}
	// An operator-trusted allowlist entry (source_security.allow_internal_hosts)
	// exempts a host from the internal-address checks below. Checked after the
	// zone-ID rejection above so an allowlist can never re-open that bypass —
	// ParseAllowlist forbids '%' in an entry, so a zone-ID host matches nothing
	// here regardless. A hostname entry exempts this literal-host layer only;
	// for http/https the resolved IP is re-checked at dial time against the
	// IP/CIDR entries (guardedDialContext), which no hostname entry satisfies.
	if activeAllowlist().AllowsHost(host) {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return fmt.Errorf("host %q is a private or internal address; refusing to contact it (permit it via source_security.allow_internal_hosts): %w", host, ErrBlockedHost)
		}
		return nil
	}
	if host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("host %q is a private or internal address; refusing to contact it (permit it via source_security.allow_internal_hosts): %w", host, ErrBlockedHost)
	}
	return nil
}
