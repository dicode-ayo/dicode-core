package git

import (
	"context"
	"strings"
	"testing"

	gogittransport "github.com/go-git/go-git/v5/plumbing/transport"
)

// Tests for the ListBranches SSRF guard (#475). ListBranches is reachable
// from the REST API with a caller-supplied URL, so it must refuse to contact
// loopback/private/link-local/internal hosts before any dial happens.
//
// Hermetic: private-host cases must fail in validation (never reaching the
// "list remote" dial stage), and the public-host case uses a pre-cancelled
// context so go-git aborts before touching the network.

func TestListBranches_RejectsPrivateHosts(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/repo.git",
		"http://127.0.0.1:8080/repo.git",
		"https://10.0.0.8/repo.git",
		"https://172.16.0.1/repo.git",
		"https://192.168.1.10/repo.git",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/repo.git",
		"http://[fe80::1]/repo.git",
		"http://[fd00::1]/repo.git",
		"http://0.0.0.0/repo.git",
		"http://localhost/repo.git",
		"http://localhost:3000/repo.git",
		"https://gitserver.local/repo.git",
		"http://metadata.google.internal/computeMetadata/v1",
		"https://foo.corp.internal/repo.git",
	}
	for _, u := range cases {
		_, err := ListBranches(context.Background(), u, "")
		if err == nil {
			t.Errorf("ListBranches(%q) = nil error, want private-host rejection", u)
			continue
		}
		if !strings.Contains(err.Error(), "private or internal") {
			t.Errorf("ListBranches(%q) error = %q, want private/internal host rejection", u, err)
		}
		// "list remote:" wraps errors from the dial stage — its presence
		// would mean validation was bypassed and a connection was attempted.
		if strings.Contains(err.Error(), "list remote") {
			t.Errorf("ListBranches(%q) reached the dial stage: %q", u, err)
		}
	}
}

func TestValidateRemoteHost_AllowsPublicHosts(t *testing.T) {
	cases := []string{
		"https://github.com/example/repo.git",
		"http://gitlab.example.com/group/repo.git",
		"https://8.8.8.8/repo.git",
		"ssh://git@github.com/example/repo.git",
		"git@github.com:example/repo.git",
		"git://example.com/repo.git",
	}
	for _, u := range cases {
		ep, err := gogittransport.NewEndpoint(u)
		if err != nil {
			t.Fatalf("NewEndpoint(%q): %v", u, err)
		}
		if err := validateRemoteHost(ep); err != nil {
			t.Errorf("validateRemoteHost(%q) = %v, want nil", u, err)
		}
	}
}

func TestValidateRemoteHost_RejectsMissingHost(t *testing.T) {
	// file:// endpoints carry no remote host — the guard must reject them
	// even though the webui scheme allowlist also blocks file:// upstream.
	ep, err := gogittransport.NewEndpoint("file:///etc/passwd")
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	if err := validateRemoteHost(ep); err == nil {
		t.Error("validateRemoteHost(file:///etc/passwd) = nil, want no-host rejection")
	}
}

// TestListBranches_PublicHostReachesDialStage proves a legitimate public URL
// passes the host guard: with a pre-cancelled context the call must fail at
// the "list remote" stage (wrapped context error), not in validation. No
// network traffic occurs because the transport aborts on the dead context.
func TestListBranches_PublicHostReachesDialStage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ListBranches(ctx, "https://github.com/example/repo.git", "")
	if err == nil {
		t.Fatal("ListBranches with cancelled ctx returned nil error")
	}
	if strings.Contains(err.Error(), "private or internal") {
		t.Fatalf("public host was rejected by the SSRF guard: %v", err)
	}
	if !strings.Contains(err.Error(), "list remote") {
		t.Fatalf("expected failure from the dial stage (list remote), got: %v", err)
	}
}
