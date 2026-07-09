package gitops

import (
	"context"
	"fmt"
	"net"
	stdhttp "net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// cgnatBlock is RFC 6598 shared address space (100.64.0.0/10), used inside
// carrier-grade NAT and cloud/provider networks. net.IP.IsPrivate() only
// covers RFC 1918 (v4) and RFC 4193 (v6) and does not include it, but it is
// no less internal-only in practice and is a known SSRF pivot range.
var cgnatBlock = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// dialTimeout and dialKeepAlive match the values net/http.DefaultTransport's
// own dialer uses. dialTCP (below) replaces that dialer entirely, so without
// restating these explicitly a dial to a black-holed/firewalled address
// would hang for the OS's own TCP connect timeout (minutes, not seconds)
// instead of the 30s the rest of the codebase already relies on.
const (
	dialTimeout   = 30 * time.Second
	dialKeepAlive = 30 * time.Second
)

// IsBlockedIP reports whether ip falls in a loopback, private, link-local,
// unspecified, multicast, or carrier-grade-NAT (100.64.0.0/10, RFC 6598)
// range — the set dicode refuses to let a git remote resolve to (SSRF
// guard, #475/#481). Handles IPv4, IPv6, and IPv4-mapped IPv6 addresses via
// the net.IP predicates' own normalisation.
//
// Known residual gap: this does not decode 6to4 (2002::/16) or Teredo
// (2001::/32) tunnelling addresses that embed a private/loopback IPv4
// payload — those addresses look like ordinary global-unicast IPv6 to
// net.IP's predicates. Exploiting that requires the resolving host to also
// have 6to4/Teredo routing enabled, which is disabled by default on the
// server platforms dicode runs on; left undecoded rather than adding
// bespoke tunnelling-format parsing for a narrow, non-default surface.
func IsBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		cgnatBlock.Contains(ip)
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
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}
	return d.DialContext(ctx, network, address)
}

// configuredProxyHosts returns the hostnames (without port) of the
// process's configured HTTP and HTTPS proxies, from the HTTPS_PROXY/
// HTTP_PROXY environment variables via the same stdhttp.ProxyFromEnvironment
// logic net/http itself uses to pick a proxy. Re-read on every call rather
// than cached: the lookup is just a couple of env-var reads (cheap relative
// to a TCP dial), and caching would otherwise permanently freeze in
// whatever the first caller happened to observe — including in tests,
// which all share one process. A var so tests can stub it directly.
var configuredProxyHosts = func() map[string]bool {
	hosts := map[string]bool{}
	for _, scheme := range []string{"http", "https"} {
		proxyURL, err := stdhttp.ProxyFromEnvironment(&stdhttp.Request{URL: &url.URL{Scheme: scheme}})
		if err != nil || proxyURL == nil || proxyURL.Host == "" {
			continue
		}
		if h, _, err := net.SplitHostPort(proxyURL.Host); err == nil {
			hosts[h] = true
		} else {
			hosts[proxyURL.Host] = true
		}
	}
	return hosts
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

	// When HTTPS_PROXY/HTTP_PROXY is configured, this is the address dicode
	// actually dials for every request through this transport — not the
	// final destination, which the proxy tunnels to separately and this
	// guard never observes. The proxy itself is an operator-configured,
	// trusted egress path, and is very commonly on a private/loopback
	// address (a local Squid instance, a corporate gateway) — blocking it
	// here would break git operations entirely whenever a private-IP proxy
	// is configured, which is the common case, not the exception. This
	// exemption doesn't reduce protection for the tunnelled destination:
	// that hop was already unguarded when proxied (see
	// InstallSSRFGuardedTransport's doc comment) — exempting the proxy hop
	// itself doesn't remove any check that applied to it before.
	if configuredProxyHosts()[host] {
		return dialTCP(ctx, network, address)
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
// client installed here.
//
// That gap is now only partial, not total: ValidateRemoteHost's
// literal-host check (hostguard.go) is invoked from CloneOrPull itself —
// the actual clone/pull path, not just pkg/source/git's ListBranches — so
// every scheme it understands (http(s), ssh, and the git@host:path SCP
// shorthand) gets rejected before any dial when the host is a loopback/
// private/link-local/internal literal or matches a blocked hostname suffix
// (#489). ssh:// and SCP-shorthand remotes get that literal-host check as
// their ONLY guard, since they never reach this dial-time layer: a hostname
// that passes the literal check but resolves — now, or later via DNS-rebind
// — to a blocked address is still uncaught for ssh, unlike http/https where
// guardedDialContext closes exactly that gap. That residual, smaller gap is
// a known limitation, not fixed here; closing it would mean either building
// a dedicated guarded dialer for go-git's ssh transport or resolving and
// re-checking the host before handing off to it, both bigger than #489's
// scope. The "git" scheme itself is separately rejected outright by
// pkg/taskset.ValidateRefURL (#486), so it never reaches CloneOrPull with a
// caller-supplied host in practice.
//
// When an outbound proxy is configured (HTTPS_PROXY/HTTP_PROXY), the
// dialer connects to the proxy's address, not the final target's — the
// standard library's ProxyFromEnvironment support composes with a custom
// Transport.DialContext by having it dial the proxy, with the proxy then
// tunnelling to the real destination. guardedDialContext exempts that proxy
// hop from the private/internal-address check entirely (see its own doc
// comment) rather than validating it: the proxy is an operator-configured,
// trusted egress path and is very commonly on a private/loopback address
// itself, so checking it the same way as an arbitrary git remote would
// break git operations whenever a private-IP proxy is configured — the
// common case, not the exception. Operators running dicode behind an
// egress proxy still rely on the proxy's own egress controls for the
// tunnelled destination, same as before this change — this only stops the
// proxy hop itself from being falsely rejected.
func InstallSSRFGuardedTransport() {
	installGuardOnce.Do(func() {
		// http.DefaultTransport is documented as a *http.Transport in every
		// Go release, but this runs from an unconditional package init(), so
		// fail open to a fresh, default-shaped Transport rather than panic
		// the whole binary at startup if that ever stops being true (e.g. a
		// test harness or future stdlib change replaces it).
		var t *stdhttp.Transport
		if dt, ok := stdhttp.DefaultTransport.(*stdhttp.Transport); ok {
			t = dt.Clone()
		} else {
			t = &stdhttp.Transport{Proxy: stdhttp.ProxyFromEnvironment}
		}
		t.DialContext = guardedDialContext

		c := &stdhttp.Client{Transport: t}
		client.InstallProtocol("http", githttp.NewClient(c))
		client.InstallProtocol("https", githttp.NewClient(c))
	})
}

func init() {
	InstallSSRFGuardedTransport()
}
