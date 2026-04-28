package daemon

import (
	"testing"

	"github.com/dicode/dicode/pkg/config"
)

// TestResolveDataDir documents the resolution order the Docker image
// relies on: the `ENV DICODE_DATA_DIR=/data` line in the runtime stage
// only redirects daemon state into the mounted volume because this
// helper consults the env var when cfg.DataDir is empty. Regressing
// this order would silently move SQLite + sources into the container's
// writable layer again — exactly the bug PR #227 fixed.
func TestResolveDataDir(t *testing.T) {
	cases := []struct {
		name    string
		cfgDir  string
		envVal  string // empty means unset
		homeDir string // overrides HOME for this case
		want    string
	}{
		{
			name:   "cfg wins over env",
			cfgDir: "/from-config",
			envVal: "/from-env",
			want:   "/from-config",
		},
		{
			name:   "env wins when cfg is empty",
			cfgDir: "",
			envVal: "/from-env",
			want:   "/from-env",
		},
		{
			name:    "home fallback when both empty",
			cfgDir:  "",
			envVal:  "",
			homeDir: "/tmp/fakehome",
			want:    "/tmp/fakehome/.dicode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv auto-restores the prior value on test teardown.
			// Empty string is treated as unset by os.Getenv (returns "").
			t.Setenv("DICODE_DATA_DIR", tc.envVal)
			if tc.homeDir != "" {
				t.Setenv("HOME", tc.homeDir)
			}

			got, err := resolveDataDir(&config.Config{DataDir: tc.cfgDir})
			if err != nil {
				t.Fatalf("resolveDataDir: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
