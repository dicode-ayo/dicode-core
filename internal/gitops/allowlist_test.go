package gitops

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

var errStubDial = errors.New("stub: dial reached")

// withAllowlist installs a parsed allowlist for the duration of the test and
// resets to the fail-closed default on cleanup, so no test can leave the
// process-wide allowlist dirty for another.
func withAllowlist(t *testing.T, entries ...string) {
	t.Helper()
	a, err := ParseAllowlist(entries)
	if err != nil {
		t.Fatalf("ParseAllowlist(%v) failed: %v", entries, err)
	}
	SetInternalHostAllowlist(a)
	t.Cleanup(func() { SetInternalHostAllowlist(nil) })
}

func TestParseAllowlist_Classification(t *testing.T) {
	a, err := ParseAllowlist([]string{
		"git.corp.internal",
		"10.0.0.0/8",
		"192.168.1.5",
		"2001:db8::/32",
		"GIT.UPPER.INTERNAL.", // case + trailing dot normalised
		"",                    // skipped
		"  spaced.internal  ", // trimmed
	})
	if err != nil {
		t.Fatalf("ParseAllowlist failed: %v", err)
	}
	// Hostname entries: exact, case-folded, trailing-dot-trimmed matches.
	for _, h := range []string{"git.corp.internal", "git.upper.internal", "spaced.internal"} {
		if !a.AllowsHost(h) {
			t.Errorf("AllowsHost(%q) = false, want true", h)
		}
	}
	// Hostname entry must NOT authorise an IP (rebind guard).
	if a.AllowsIP(net.ParseIP("172.16.0.1")) {
		t.Error("AllowsIP(172.16.0.1) = true, want false (no CIDR covers it)")
	}
	// CIDR + bare-IP entries authorise both layers.
	if !a.AllowsIP(net.ParseIP("10.5.5.5")) {
		t.Error("AllowsIP(10.5.5.5) = false, want true (10.0.0.0/8)")
	}
	if !a.AllowsHost("10.5.5.5") {
		t.Error("AllowsHost(10.5.5.5) = false, want true (IP literal in CIDR)")
	}
	if !a.AllowsIP(net.ParseIP("192.168.1.5")) {
		t.Error("AllowsIP(192.168.1.5) = false, want true (bare IP entry)")
	}
	if a.AllowsIP(net.ParseIP("192.168.1.6")) {
		t.Error("AllowsIP(192.168.1.6) = true, want false (bare IP is /32)")
	}
	if !a.AllowsIP(net.ParseIP("2001:db8::1")) {
		t.Error("AllowsIP(2001:db8::1) = false, want true (v6 CIDR)")
	}
}

func TestParseAllowlist_RejectsMalformed(t *testing.T) {
	cases := []string{
		"http://git.internal", // scheme
		"git.internal:22",     // port
		"fe80::1%eth0",        // zone-ID
		"[::1]",               // bracketed literal
		"user@git.internal",   // userinfo
		"git.internal/path",   // path
		"10.0.0.0/999",        // bad CIDR
		"not a host",          // space
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := ParseAllowlist([]string{c}); err == nil {
				t.Errorf("ParseAllowlist(%q) = nil error, want rejection", c)
			}
		})
	}
}

// TestParseAllowlist_NoSubstringMatch proves an entry matches only a full host,
// never as a substring — a listed "corp.internal" must not authorise the
// attacker-controlled "corp.internal.evil.com" or "evilcorp.internal".
func TestParseAllowlist_NoSubstringMatch(t *testing.T) {
	a, _ := ParseAllowlist([]string{"corp.internal"})
	for _, h := range []string{"corp.internal.evil.com", "evilcorp.internal", "xcorp.internal"} {
		if a.AllowsHost(h) {
			t.Errorf("AllowsHost(%q) = true, want false (substring must not match)", h)
		}
	}
}

// TestValidateRemoteHost_AllowlistedHost proves the literal-host layer honours
// an allowlisted hostname across ssh and https (and SCP-shorthand), while an
// unlisted internal host is still rejected.
func TestValidateRemoteHost_AllowlistedHost(t *testing.T) {
	withAllowlist(t, "git.corp.internal", "10.0.0.0/8")

	allowed := []string{
		"ssh://git@git.corp.internal/org/repo.git",
		"https://git.corp.internal/org/repo.git",
		"git@git.corp.internal:org/repo.git",
		"https://10.1.2.3/repo.git", // IP literal inside listed CIDR
	}
	for _, u := range allowed {
		if err := ValidateRemoteHost(u); err != nil {
			t.Errorf("ValidateRemoteHost(%q) = %v, want nil (allowlisted)", u, err)
		}
	}

	// A different internal host is still rejected, and the message names the key.
	rejected := "ssh://git@other.corp.internal/repo.git"
	err := ValidateRemoteHost(rejected)
	if err == nil {
		t.Fatalf("ValidateRemoteHost(%q) = nil, want rejection (not allowlisted)", rejected)
	}
	if !strings.Contains(err.Error(), "source_security.allow_internal_hosts") {
		t.Errorf("rejection error = %q, want it to name the config key", err)
	}
}

// TestValidateRemoteHost_AllowlistDoesNotUndoZoneID proves an allowlist can't
// re-open the zone-ID bypass hostguard.go closes: even with a broad CIDR
// listed, a zone-scoped IPv6 literal is still rejected outright.
func TestValidateRemoteHost_AllowlistDoesNotUndoZoneID(t *testing.T) {
	withAllowlist(t, "fe80::/10")
	err := ValidateRemoteHost("ssh://git@[fe80::1%25eth0]/repo.git")
	if err == nil {
		t.Fatal("ValidateRemoteHost with zone-ID = nil, want rejection despite allowlist")
	}
}

// TestGuardedDialContext_AllowlistedResolvedIP proves the dial-time layer
// honours an allowlisted CIDR for the resolved IP (http/https).
func TestGuardedDialContext_AllowlistedResolvedIP(t *testing.T) {
	withAllowlist(t, "10.0.0.0/8")
	var dialed string
	withStubs(t,
		func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.1.2.3")}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = address
			return nil, errStubDial
		},
	)
	_, err := guardedDialContext(context.Background(), "tcp", "git.corp.internal:443")
	if err != errStubDial {
		t.Fatalf("expected stub dial to be reached (IP allowlisted), got %v", err)
	}
	if dialed != "10.1.2.3:443" {
		t.Errorf("dialed %q, want the resolved allowlisted IP", dialed)
	}
}

// TestGuardedDialContext_HostnameEntryDoesNotAuthorizeResolvedIP is the trap
// test: an allowlisted *hostname* (no CIDR) must NOT let the dial-time layer
// through to a private resolved IP. This is the DNS-rebind case for http/https
// — the whole point of keeping the two layers keyed on different values.
func TestGuardedDialContext_HostnameEntryDoesNotAuthorizeResolvedIP(t *testing.T) {
	withAllowlist(t, "git.corp.internal") // hostname only, no CIDR
	withStubs(t,
		func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		},
		failIfCalledDial(t),
	)
	_, err := guardedDialContext(context.Background(), "tcp", "git.corp.internal:443")
	if err == nil {
		t.Fatal("expected rejection: a hostname entry must not authorise its resolved private IP")
	}
	if !strings.Contains(err.Error(), "source_security.allow_internal_hosts") {
		t.Errorf("rejection error = %q, want it to name the config key", err)
	}
}

// TestGuardedDialContext_NonAllowlistedResolvedIPStillBlocked proves an empty
// allowlist (or one that doesn't cover the IP) still rejects at dial time.
func TestGuardedDialContext_NonAllowlistedResolvedIPStillBlocked(t *testing.T) {
	withAllowlist(t, "10.0.0.0/8")
	withStubs(t,
		func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("192.168.1.1")}, nil // not in 10/8
		},
		failIfCalledDial(t),
	)
	_, err := guardedDialContext(context.Background(), "tcp", "sneaky.example.test:443")
	if err == nil {
		t.Fatal("expected rejection: resolved IP not covered by allowlist")
	}
}
