package gitops

import (
	"context"
	"fmt"
	"net"
	"testing"
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
// network or DNS.
func withStubs(t *testing.T, resolve func(ctx context.Context, host string) ([]net.IP, error), dial func(ctx context.Context, network, address string) (net.Conn, error)) {
	t.Helper()
	origResolve, origDial := resolveHost, dialTCP
	if resolve != nil {
		resolveHost = resolve
	}
	if dial != nil {
		dialTCP = dial
	}
	t.Cleanup(func() {
		resolveHost = origResolve
		dialTCP = origDial
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
