package main

import (
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/ipc"
)

func TestFormatRelayStatusLine(t *testing.T) {
	cases := []struct {
		name string
		in   ipc.RelayStatus
		want string
	}{
		{"disabled", ipc.RelayStatus{}, "Relay: disabled"},
		{"not-linked", ipc.RelayStatus{Enabled: true}, "Relay: not linked (run `dicode relay login`)"},
		{"linked", ipc.RelayStatus{Enabled: true, Linked: true, GithubLogin: "octocat"}, "Relay: linked to @octocat"},
		{"linked-no-login", ipc.RelayStatus{Enabled: true, Linked: true}, "Relay: linked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatRelayStatusLine(tc.in)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestBuildDashboardURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "https://relay.dicode.app/dashboard/claim"},
		{"http://localhost:5553", "http://localhost:5553/dashboard/claim"},
		{"http://localhost:5553/", "http://localhost:5553/dashboard/claim"},
	}
	for _, tc := range cases {
		got := buildDashboardURL(tc.in)
		if got != tc.want {
			t.Fatalf("in=%q got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortUUID(t *testing.T) {
	if shortUUID("abc") != "abc" {
		t.Fatalf("short uuid should pass through")
	}
	long := strings.Repeat("a", 64)
	got := shortUUID(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
	if len(got) > 16 {
		t.Fatalf("shortened uuid too long: %q", got)
	}
}
