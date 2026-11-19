//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUIDMatches_NoRootExemption(t *testing.T) {
	cases := []struct {
		fileUID uint32
		caller  int
		want    bool
	}{
		{1000, 1000, true},
		{0, 0, true},
		{1000, 0, false},
		{0, 1000, false},
		{1000, 1001, false},
		{0xffffffff, -1, false},
	}
	for _, tc := range cases {
		if got := uidMatches(tc.fileUID, tc.caller); got != tc.want {
			t.Errorf("uidMatches(%d, %d) = %v, want %v", tc.fileUID, tc.caller, got, tc.want)
		}
	}
}

func TestConfiguredSearchTargets_RootRefusesOtherUsersConfig(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown the config to another user")
	}
	cfgPath := filepath.Join(t.TempDir(), "dicode.yaml")
	if err := os.WriteFile(cfgPath, []byte("spec:\n  entries: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(cfgPath, 4242, 4242); err != nil {
		t.Fatal(err)
	}
	_, err := configuredSearchTargets(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "not a regular file owned by the current user") {
		t.Fatalf("err = %v, want refusal of another user's config", err)
	}
}
