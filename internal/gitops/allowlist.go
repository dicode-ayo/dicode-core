package gitops

import (
	"fmt"
	"net"
	"strings"
	"sync/atomic"
)

// Allowlist is the set of internal git-remote hosts and networks an operator
// has explicitly trusted (source_security.allow_internal_hosts, #537). It is
// the single source of truth both SSRF guard layers consult, but they key on
// different values and so honour different entry kinds:
//
//   - ValidateRemoteHost (hostguard.go) matches the literal host string, so it
//     honours BOTH hostname entries (exact match) and IP/CIDR entries (when the
//     literal host is itself an IP inside a listed range).
//   - guardedDialContext (dialguard.go) matches the resolved connection IP, so
//     it honours ONLY IP/CIDR entries. A hostname entry never authorises an
//     address that hostname happens to resolve to — closing the DNS-rebind gap
//     for http/https even when the hostname is allowlisted.
//
// Consequence, documented for operators: an ssh/SCP-shorthand remote to an
// internal host is authorised by a hostname entry alone (it only ever reaches
// the literal-host layer), but an http/https remote also needs the resolved
// IP's range listed as a CIDR, because the dial-time layer only ever sees IPs.
type Allowlist struct {
	hosts map[string]struct{}
	nets  []*net.IPNet
}

// emptyAllowlist is the fail-closed default returned whenever no allowlist has
// been configured: it authorises nothing, so absent config reproduces exactly
// the pre-#537 behaviour.
var emptyAllowlist = &Allowlist{}

// ParseAllowlist classifies each entry as a CIDR, a bare IP (normalised to a
// single-address CIDR), or a hostname (lowercased and stripped of a trailing
// FQDN-root dot to line up with ValidateRemoteHost's own normalisation). It
// rejects entries that carry a scheme, port, path, userinfo, brackets, or an
// IPv6 zone-ID, so an allowlist string can never smuggle in the exact tricks
// the guards defend against. A nil/empty slice yields the fail-closed empty
// allowlist.
func ParseAllowlist(entries []string) (*Allowlist, error) {
	a := &Allowlist{hosts: map[string]struct{}{}}
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			_, n, err := net.ParseCIDR(e)
			if err != nil {
				return nil, fmt.Errorf("source_security.allow_internal_hosts: invalid CIDR %q: %w", raw, err)
			}
			a.nets = append(a.nets, n)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			a.nets = append(a.nets, singleIPNet(ip))
			continue
		}
		host := strings.TrimRight(strings.ToLower(e), ".")
		if host == "" || strings.ContainsAny(host, ":%[]@ \t") {
			return nil, fmt.Errorf("source_security.allow_internal_hosts: %q is not a bare hostname, IP address, or CIDR", raw)
		}
		a.hosts[host] = struct{}{}
	}
	return a, nil
}

// singleIPNet wraps a bare IP as a /32 (v4) or /128 (v6) network so it can be
// stored uniformly alongside CIDR entries.
func singleIPNet(ip net.IP) *net.IPNet {
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
}

// AllowsHost reports whether the literal host is allowlisted: either it matches
// a hostname entry exactly, or it is an IP literal inside a listed range. host
// is re-normalised (bracket-stripped, lowercased, trailing-dot-trimmed) so a
// caller that skipped normalisation cannot bypass an entry by casing or a
// trailing dot.
func (a *Allowlist) AllowsHost(host string) bool {
	if a == nil {
		return false
	}
	h := strings.TrimRight(strings.ToLower(strings.Trim(host, "[]")), ".")
	if h == "" {
		return false
	}
	if _, ok := a.hosts[h]; ok {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return a.allowsIP(ip)
	}
	return false
}

// AllowsIP reports whether ip falls inside a listed IP/CIDR entry. Hostname
// entries are deliberately never consulted here: the dial-time layer sees only
// resolved IPs, so a hostname entry must not authorise an arbitrary IP it
// resolves to (the DNS-rebind case).
func (a *Allowlist) AllowsIP(ip net.IP) bool {
	return a.allowsIP(ip)
}

func (a *Allowlist) allowsIP(ip net.IP) bool {
	if a == nil || ip == nil {
		return false
	}
	for _, n := range a.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// configuredAllowlist holds the process-wide allowlist. An atomic.Pointer
// rather than a plain field because guardedDialContext reads it from every
// dial goroutine while the daemon writes it once at startup. A nil pointer
// (never set) resolves to the fail-closed empty allowlist.
var configuredAllowlist atomic.Pointer[Allowlist]

// SetInternalHostAllowlist installs the operator-configured allowlist that both
// SSRF guard layers consult. The daemon calls this once at startup from
// source_security.allow_internal_hosts. Passing nil restores the fail-closed
// default, so tests can reset cleanly.
func SetInternalHostAllowlist(a *Allowlist) {
	configuredAllowlist.Store(a)
}

// activeAllowlist returns the configured allowlist, or the fail-closed empty
// one when none has been set.
func activeAllowlist() *Allowlist {
	if a := configuredAllowlist.Load(); a != nil {
		return a
	}
	return emptyAllowlist
}
