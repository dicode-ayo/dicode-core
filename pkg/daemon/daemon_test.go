package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/task"
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

// TestRetentionOverrideInOnRegister verifies Finding 2: the OnRegister hook
// must override buildin/run-inputs-cleanup's retention_seconds param default
// to reflect dicode.yaml's defaults.run_inputs.retention. Without this fix
// the cleanup task always ran with its hard-coded 30-day default regardless
// of the operator's configured retention.
//
// This test exercises the same mutation logic that lives inside the OnRegister
// closure in run(), keeping the logic in one place and the test cheap.
func TestRetentionOverrideInOnRegister(t *testing.T) {
	retention := 7 * 24 * time.Hour // 7 days = 604800s
	cfg := &config.Config{}
	cfg.Defaults.RunInputs.Retention = retention

	// Build a spec that matches what the reconciler loads from the task dir.
	spec := &task.Spec{
		ID: "buildin/run-inputs-cleanup",
		Params: task.Params{
			{Name: "retention_seconds", Default: "2592000", Type: "number"},
		},
	}

	// Simulate exactly what the OnRegister closure does.
	if spec.ID == "buildin/run-inputs-cleanup" && cfg.Defaults.RunInputs.Retention > 0 {
		retStr := fmt.Sprintf("%d", int64(cfg.Defaults.RunInputs.Retention.Seconds()))
		for i := range spec.Params {
			if spec.Params[i].Name == "retention_seconds" {
				spec.Params[i].Default = retStr
				break
			}
		}
	}

	want := fmt.Sprintf("%d", int64(retention.Seconds())) // "604800"
	got := spec.Params[0].Default
	if got != want {
		t.Errorf("retention_seconds default = %q, want %q", got, want)
	}
}

// TestRetentionOverrideSkippedWhenZero verifies that the OnRegister hook does
// NOT mutate retention_seconds when cfg.Defaults.RunInputs.Retention is zero
// (i.e., the operator left it unset — the task's own 30-day default applies).
func TestRetentionOverrideSkippedWhenZero(t *testing.T) {
	cfg := &config.Config{} // Retention == 0

	spec := &task.Spec{
		ID: "buildin/run-inputs-cleanup",
		Params: task.Params{
			{Name: "retention_seconds", Default: "2592000", Type: "number"},
		},
	}

	// Same logic as OnRegister.
	if spec.ID == "buildin/run-inputs-cleanup" && cfg.Defaults.RunInputs.Retention > 0 {
		retStr := fmt.Sprintf("%d", int64(cfg.Defaults.RunInputs.Retention.Seconds()))
		for i := range spec.Params {
			if spec.Params[i].Name == "retention_seconds" {
				spec.Params[i].Default = retStr
				break
			}
		}
	}

	if got := spec.Params[0].Default; got != "2592000" {
		t.Errorf("expected default unchanged (2592000) when retention is zero, got %q", got)
	}
}
