package gitops

import (
	"context"
	"fmt"
	"net"
	stdhttp "net/http"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// IsBlockedIP reports whether ip falls in a loopback, private, link-local,
// unspecified, or multicast range — the set dicode refuses to let a git
// remote resolve to (SSRF guard, #475/#481). Handles IPv4, IPv6, and
// IPv4-mapped IPv6 addresses via the net.IP predicates' own normalisation.
func IsBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// resolveHost looks up the IP addresses for host. Extracted as a var so
// tests can inject a fake resolver and exercise the DNS-rebind rejection
// path without any real network access.
var resolveHost = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, nil
}

// dialTCP performs the actual TCP connect once a target IP has been
// resolved and cleared. A var so tests can stub it out and assert on the
// address they were asked to dial, without touching the network.
var dialTCP = func(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// guardedDialContext replaces the default DialContext used by go-git's
// installed HTTP(S) transport (see InstallSSRFGuardedTransport). It closes
// the DNS-rebind gap left by validateRemoteHost's literal-host-only check
// (#475/#480): a hostname that looks public but resolves — now, or later on
// a TTL flip — to a loopback/private/link-local/internal address is
// rejected here, at the moment dicode would actually connect to it.
//
// Resolution happens exactly once, inside this function: every candidate
// address is checked against IsBlockedIP *before* any candidate is dialed,
// and the connection is then made directly to the validated IP (never by
// re-resolving the hostname), so there is no window between "checked" and
// "connected" for a rebind to land in. If any candidate resolves to a
// blocked address, the whole hostname is rejected — a mix of public and
// private answers is itself a signal of a hostile or misconfigured DNS
// answer, so the safe candidates are not used either.
func guardedDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial guard: split host:port %q: %w", address, err)
	}

	var candidates []net.IP
	if ip := net.ParseIP(host); ip != nil {
		candidates = []net.IP{ip}
	} else {
		resolved, err := resolveHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("dial guard: resolve %q: %w", host, err)
		}
		candidates = resolved
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("dial guard: host %q resolved to no addresses", host)
	}
	for _, ip := range candidates {
		if IsBlockedIP(ip) {
			return nil, fmt.Errorf("dial guard: %q resolves to private/internal address %s; refusing to connect", host, ip)
		}
	}

	var lastErr error
	for _, ip := range candidates {
		conn, err := dialTCP(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

var installGuardOnce sync.Once

// InstallSSRFGuardedTransport installs a process-wide go-git HTTP(S)
// transport whose dialer rejects any resolved connection address in the
// loopback/private/link-local/unspecified/multicast ranges — the
// dial-time, DNS-rebind-proof half of the SSRF guard whose literal-host
// half is validateRemoteHost (pkg/source/git). See #475/#480/#481.
//
// go-git's client.InstallProtocol registry is process-global, so this is
// idempotent (guarded by sync.Once): safe to call from multiple entry
// points, and called automatically by this package's init so every binary
// that imports internal/gitops (git source, taskset loader) gets the guard
// without needing to remember to wire it up.
//
// Only "http" and "https" are re-registered. This guard does NOT cover the
// "ssh" or "git" schemes: both dial through go-git's own transports (an
// SSH client and a bare TCP `net.Dial` respectively — the latter with no
// injectable dialer at all) with no shared choke point with the HTTP(S)
// client installed here. A `ref.url: git://<attacker-controlled host>` is
// therefore NOT protected by this guard, at either the literal-host layer
// (validateRemoteHost is only ever invoked from pkg/source/git's
// ListBranches, not from any clone/pull path) or this dial-time layer —
// tracked separately as a follow-up, since closing it means either
// rejecting the "git" scheme in taskset ref validation or building a
// dedicated guarded transport for it, both bigger than this issue's scope.
//
// When an outbound proxy is configured (HTTPS_PROXY/HTTP_PROXY), the
// dialer connects to the proxy's address, not the final target's — the
// standard library's ProxyFromEnvironment support composes with a custom
// Transport.DialContext by having it dial the proxy, with the proxy then
// tunnelling to the real destination. This guard therefore validates the
// proxy endpoint in that case, not the tunnelled target; operators running
// dicode behind an egress proxy rely on the proxy's own egress controls for
// the tunnelled destination, same as before this change.
func InstallSSRFGuardedTransport() {
	installGuardOnce.Do(func() {
		t := stdhttp.DefaultTransport.(*stdhttp.Transport).Clone()
		t.DialContext = guardedDialContext

		c := &stdhttp.Client{Transport: t}
		client.InstallProtocol("http", githttp.NewClient(c))
		client.InstallProtocol("https", githttp.NewClient(c))
	})
}

func init() {
	InstallSSRFGuardedTransport()
}
