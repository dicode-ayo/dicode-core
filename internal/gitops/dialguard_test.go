package gitops

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
)

func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"private 10.x", "10.1.2.3", true},
		{"private 172.16.x", "172.16.0.1", true},
		{"private 192.168.x", "192.168.1.1", true},
		{"link-local v4", "169.254.1.1", true},
		{"link-local v6", "fe80::1", true},
		{"unique-local v6", "fd00::1", true},
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		{"multicast v4", "224.0.0.1", true},
		{"cgnat range start", "100.64.0.0", true},
		{"cgnat range mid", "100.100.1.1", true},
		{"cgnat range end", "100.127.255.255", true},
		{"just below cgnat range", "100.63.255.255", false},
		{"just above cgnat range", "100.128.0.0", false},
		{"ipv4-mapped loopback", "::ffff:127.0.0.1", true},
		{"ipv4-mapped private", "::ffff:10.0.0.5", true},
		{"public v4", "8.8.8.8", false},
		{"public v6", "2606:4700:4700::1111", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) returned nil", tc.ip)
			}
			if got := IsBlockedIP(ip); got != tc.want {
				t.Errorf("IsBlockedIP(%q) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

// withStubs temporarily swaps resolveHost/dialTCP for the duration of the
// test and restores the originals on cleanup, so tests never touch the real
// network or DNS. It also stubs configuredProxyHosts to report no proxy —
// without this, these tests' outcomes would depend on whatever HTTPS_PROXY/
// HTTP_PROXY happens to be set in the environment they run in (e.g. a
// sandboxed dev environment routing all HTTPS through a local proxy)
// instead of on the guard's own logic. Tests that specifically exercise the
// proxy exemption override configuredProxyHosts again after calling this.
//
// Not safe for use with t.Parallel(): the swapped vars are package-level and
// unsynchronized, shared by every test in this file.
func withStubs(t *testing.T, resolve func(ctx context.Context, host string) ([]net.IP, error), dial func(ctx context.Context, network, address string) (net.Conn, error)) {
	t.Helper()
	origResolve, origDial, origProxyHosts := resolveHost, dialTCP, configuredProxyHosts
	if resolve != nil {
		resolveHost = resolve
	}
	if dial != nil {
		dialTCP = dial
	}
	configuredProxyHosts = func() map[string]bool { return map[string]bool{} }
	t.Cleanup(func() {
		resolveHost = origResolve
		dialTCP = origDial
		configuredProxyHosts = origProxyHosts
	})
}

func failIfCalledResolve(t *testing.T) func(ctx context.Context, host string) ([]net.IP, error) {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		t.Fatalf("resolveHost should not have been called for host %q", host)
		return nil, nil
	}
}

func failIfCalledDial(t *testing.T) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		t.Fatalf("dialTCP should not have been called (address %q)", address)
		return nil, nil
	}
}

// TestGuardedDialContext_RejectsResolvedPrivateIP is the DNS-rebind case:
// a hostname that is not itself suspicious resolves to a private address.
// The guard must reject the dial without ever attempting a real connection.
func TestGuardedDialContext_RejectsResolvedPrivateIP(t *testing.T) {
	withStubs(t,
		func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil // cloud metadata endpoint
		},
		failIfCalledDial(t),
	)

	_, err := guardedDialContext(context.Background(), "tcp", "looks-public.example.test:443")
	if err == nil {
		t.Fatal("expected error for hostname resolving to a private/internal address")
	}
}

// TestGuardedDialContext_RejectsWhenAnyCandidateBlocked proves the
// fail-closed policy: if any resolved candidate is internal, the whole
// hostname is rejected — even though a public-looking candidate is also
// present. dialTCP must never be reached.
func TestGuardedDialContext_RejectsWhenAnyCandidateBlocked(t *testing.T) {
	withStubs(t,
		func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}, nil
		},
		failIfCalledDial(t),
	)

	_, err := guardedDialContext(context.Background(), "tcp", "mixed.example.test:443")
	if err == nil {
		t.Fatal("expected error when any resolved candidate is blocked")
	}
}

// TestGuardedDialContext_AllowsResolvedPublicIP proves the happy path wires
// resolution -> validation -> dial correctly, and that the connection is
// made to the validated IP (not by re-resolving the hostname a second time,
// which would reopen the rebind window).
func TestGuardedDialContext_AllowsResolvedPublicIP(t *testing.T) {
	var dialedAddress string
	withStubs(t,
		func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialedAddress = address
			return nil, fmt.Errorf("stub: no real connection")
		},
	)

	_, err := guardedDialContext(context.Background(), "tcp", "example.test:443")
	if err == nil || err.Error() != "stub: no real connection" {
		t.Fatalf("expected the stub dial error to propagate, got %v", err)
	}
	if dialedAddress != "8.8.8.8:443" {
		t.Errorf("dialed address = %q, want %q (the resolved IP, not the hostname)", dialedAddress, "8.8.8.8:443")
	}
}

// TestGuardedDialContext_IPLiteralSkipsResolver proves that when the address
// is already an IP literal (as go-git may pass after its own endpoint
// parsing), the guard validates it directly and never calls the resolver.
func TestGuardedDialContext_IPLiteralSkipsResolver(t *testing.T) {
	withStubs(t, failIfCalledResolve(t), failIfCalledDial(t))

	_, err := guardedDialContext(context.Background(), "tcp", "127.0.0.1:9418")
	if err == nil {
		t.Fatal("expected error for a loopback IP literal")
	}
}

// TestGuardedDialContext_ExemptsConfiguredProxyHost proves that a dial
// target matching a configured HTTPS_PROXY/HTTP_PROXY host bypasses
// resolution and the block check entirely — dialed as-is, unresolved. This
// is the fix for the common case where the proxy itself sits on a private
// address (a local Squid instance, a corporate gateway): without the
// exemption, guardedDialContext would reject every request through such a
// proxy, since it's the proxy's address (not the tunnelled destination's)
// that gets dialed.
func TestGuardedDialContext_ExemptsConfiguredProxyHost(t *testing.T) {
	withStubs(t, failIfCalledResolve(t), nil)
	configuredProxyHosts = func() map[string]bool {
		return map[string]bool{"internal-proxy.corp.example": true}
	}

	var dialedAddr string
	dialTCP = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialedAddr = address
		return nil, fmt.Errorf("stub: proxy dial")
	}

	_, err := guardedDialContext(context.Background(), "tcp", "internal-proxy.corp.example:3128")
	if err == nil || err.Error() != "stub: proxy dial" {
		t.Fatalf("expected the stub dial error to propagate, got %v", err)
	}
	if dialedAddr != "internal-proxy.corp.example:3128" {
		t.Errorf("dialed address = %q, want the proxy address dialed directly, unresolved", dialedAddr)
	}
}

// TestGuardedDialContext_NonProxyPrivateHostStillBlocked proves the proxy
// exemption is narrow: a private-address dial target that is NOT the
// configured proxy host is still rejected normally.
func TestGuardedDialContext_NonProxyPrivateHostStillBlocked(t *testing.T) {
	withStubs(t, failIfCalledResolve(t), failIfCalledDial(t))
	configuredProxyHosts = func() map[string]bool {
		return map[string]bool{"internal-proxy.corp.example": true}
	}

	_, err := guardedDialContext(context.Background(), "tcp", "127.0.0.1:9418")
	if err == nil {
		t.Fatal("expected a loopback IP literal to still be rejected when it isn't the configured proxy host")
	}
}

// TestGuardedDialContext_ResolveError propagates resolver failures instead
// of silently allowing the dial.
func TestGuardedDialContext_ResolveError(t *testing.T) {
	withStubs(t,
		func(ctx context.Context, host string) ([]net.IP, error) {
			return nil, fmt.Errorf("stub: lookup failed")
		},
		failIfCalledDial(t),
	)

	_, err := guardedDialContext(context.Background(), "tcp", "example.test:443")
	if err == nil {
		t.Fatal("expected resolver error to propagate")
	}
}

// TestInstallSSRFGuardedTransport_Idempotent confirms repeated calls (e.g.
// from multiple entry points, or package init plus an explicit call) don't
// panic or double-register.
func TestInstallSSRFGuardedTransport_Idempotent(t *testing.T) {
	InstallSSRFGuardedTransport()
	InstallSSRFGuardedTransport()
}

// TestDialSettings_MatchHTTPDefaultTransport pins dialTCP's Timeout/KeepAlive
// to the values net/http.DefaultTransport's own dialer used before its
// DialContext was replaced with guardedDialContext. Losing these silently
// (e.g. back to a zero-value net.Dialer) would turn a dial to a
// black-holed/firewalled address into an OS-level-timeout hang (minutes)
// instead of failing at the bound the rest of the codebase expects.
func TestDialSettings_MatchHTTPDefaultTransport(t *testing.T) {
	if dialTimeout != 30*time.Second {
		t.Errorf("dialTimeout = %v, want 30s (net/http.DefaultTransport's dialer)", dialTimeout)
	}
	if dialKeepAlive != 30*time.Second {
		t.Errorf("dialKeepAlive = %v, want 30s (net/http.DefaultTransport's dialer)", dialKeepAlive)
	}
}

// TestGuardedDialContext_FallsBackOnDialError proves the multi-candidate
// loop actually falls through to a later candidate when an earlier one
// fails to connect (as opposed to just being validated and never
// exercised): the first candidate's dial errors, the second succeeds, and
// the returned connection is the second dial's.
func TestGuardedDialContext_FallsBackOnDialError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	var dialed []string
	withStubs(t,
		func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.1"), net.ParseIP("203.0.113.2")}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			if address == "203.0.113.1:443" {
				return nil, fmt.Errorf("stub: connection refused")
			}
			return clientConn, nil
		},
	)

	conn, err := guardedDialContext(context.Background(), "tcp", "multi.example.test:443")
	if err != nil {
		t.Fatalf("expected success via the second candidate, got %v", err)
	}
	if conn != clientConn {
		t.Error("returned conn is not the stub's successful connection")
	}
	want := []string{"203.0.113.1:443", "203.0.113.2:443"}
	if len(dialed) != len(want) || dialed[0] != want[0] || dialed[1] != want[1] {
		t.Errorf("dialed = %v, want %v (both candidates tried, in order)", dialed, want)
	}
}

// TestInstallSSRFGuardedTransport_RejectsRealLoopbackViaGoGit proves the
// installed transport is actually reachable through go-git's real request
// path (client.Protocols -> githttp.NewClient -> our guarded DialContext),
// not just unit-tested in isolation. It drives an actual go-git remote list
// against an httptest.Server, which genuinely listens on 127.0.0.1 — a real
// TCP loopback address, not a stubbed one — so a rejection here can only
// come from guardedDialContext actually being wired into go-git's client.
func TestInstallSSRFGuardedTransport_RejectsRealLoopbackViaGoGit(t *testing.T) {
	InstallSSRFGuardedTransport() // idempotent; asserts explicit re-install is harmless too

	// Neutralize the proxy exemption for this test specifically: this
	// process may itself be running behind an HTTPS_PROXY on 127.0.0.1 (a
	// common sandboxed-dev-environment setup), which would otherwise make
	// configuredProxyHosts() legitimately treat the loopback test server
	// below as "the configured proxy" and exempt it — masking the very
	// rejection this test exists to prove.
	origProxyHosts := configuredProxyHosts
	configuredProxyHosts = func() map[string]bool { return map[string]bool{} }
	t.Cleanup(func() { configuredProxyHosts = origProxyHosts })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must never be reached: the dial guard should reject the connection before any HTTP request is sent")
	}))
	defer srv.Close()

	rem := gogit.NewRemote(nil, &gogitconfig.RemoteConfig{Name: "origin", URLs: []string{srv.URL}})
	_, err := rem.ListContext(context.Background(), &gogit.ListOptions{})
	if err == nil {
		t.Fatal("expected the loopback dial to be rejected")
	}
	if !strings.Contains(err.Error(), "private/internal address") {
		t.Fatalf("expected a dial-guard rejection, got: %v", err)
	}
}
