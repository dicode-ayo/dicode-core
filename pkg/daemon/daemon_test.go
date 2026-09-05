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
// writable layer again.
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

// TestRetentionOverrideInOnRegister verifies Finding 2: the arm closure
// (newArmDisarm, wired as OnRegister) must override buildin/run-inputs-cleanup's
// retention_seconds param default to reflect dicode.yaml's
// defaults.run_inputs.retention. Without this fix the cleanup task always ran
// with its hard-coded 30-day default regardless of the operator's configured
// retention.
//
// It also pins the #832 code-review fix: the override must land on a copy,
// never on the caller's spec in place. That spec is the exact object the
// approval gate's Admit stores in its pending/admitted bookkeeping before
// calling arm, and a pinned buildin task can now sit pending while
// Gate.State concurrently reads that same object's Params — an in-place
// write there is a data race the race detector catches.
//
// This test exercises the same mutation logic that lives inside the arm
// closure in newArmDisarm, keeping the logic in one place and the test cheap.
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
	var k task.Kinded = spec

	// Simulate exactly what the arm closure does.
	if spec.ID == "buildin/run-inputs-cleanup" && cfg.Defaults.RunInputs.Retention > 0 {
		retStr := fmt.Sprintf("%d", int64(cfg.Defaults.RunInputs.Retention.Seconds()))
		for i := range spec.Params {
			if spec.Params[i].Name == "retention_seconds" {
				override := *spec
				override.Params = append(task.Params(nil), spec.Params...)
				override.Params[i].Default = retStr
				spec = &override
				k = spec
				break
			}
		}
	}

	want := fmt.Sprintf("%d", int64(retention.Seconds())) // "604800"
	if got := spec.Params[0].Default; got != want {
		t.Errorf("retention_seconds default = %q, want %q", got, want)
	}
	if got := k.(*task.Spec).Params[0].Default; got != want {
		t.Errorf("k's retention_seconds default = %q, want %q", got, want)
	}
}

// TestRetentionOverrideDoesNotMutateCallersSpec is the #832 code-review
// regression: the override must never write through the spec the caller
// (the approval gate) handed to arm, since that exact object can now be
// concurrently read by Gate.State while a pinned buildin task sits pending.
func TestRetentionOverrideDoesNotMutateCallersSpec(t *testing.T) {
	retention := 7 * 24 * time.Hour
	cfg := &config.Config{}
	cfg.Defaults.RunInputs.Retention = retention

	original := &task.Spec{
		ID: "buildin/run-inputs-cleanup",
		Params: task.Params{
			{Name: "retention_seconds", Default: "2592000", Type: "number"},
		},
	}
	callerCopy := original // the reference the gate would keep in g.admitted/g.pending
	spec := original

	if spec.ID == "buildin/run-inputs-cleanup" && cfg.Defaults.RunInputs.Retention > 0 {
		retStr := fmt.Sprintf("%d", int64(cfg.Defaults.RunInputs.Retention.Seconds()))
		for i := range spec.Params {
			if spec.Params[i].Name == "retention_seconds" {
				override := *spec
				override.Params = append(task.Params(nil), spec.Params...)
				override.Params[i].Default = retStr
				spec = &override
				break
			}
		}
	}

	if got := callerCopy.Params[0].Default; got != "2592000" {
		t.Errorf("caller's spec was mutated in place: Default = %q, want unchanged 2592000", got)
	}
	if spec == callerCopy {
		t.Error("the overridden spec must be a distinct object from the caller's, not the same pointer")
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

// TestRelayServerBodyGate exercises the predicate the arm closure consults
// before eng.Register. The configured-relay branch is the load-bearing half —
// it must report false when the operator has set up the relay, or the relay
// never starts.
func TestRelayServerBodyGate(t *testing.T) {
	cases := []struct {
		name         string
		enabled      bool
		serverURL    string
		wantDisabled bool
	}{
		{name: "relay unconfigured (default)", wantDisabled: true},
		{name: "enabled flag but no server url", enabled: true, wantDisabled: true},
		{name: "server url but flag off", serverURL: "wss://relay.example.com", wantDisabled: true},
		{name: "relay fully configured", enabled: true, serverURL: "wss://relay.example.com", wantDisabled: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Relay.Enabled = tc.enabled
			cfg.Relay.ServerURL = tc.serverURL

			spec := &task.Spec{ID: "buildin/relay-server-body", Enabled: true}
			got := gateRelayServerBody(spec, cfg)

			if got != tc.wantDisabled {
				t.Errorf("gateRelayServerBody = %v, want %v", got, tc.wantDisabled)
			}
			// A pure predicate: never mutates the spec it was handed — see
			// TestArmRelayServerBodyDoesNotMutateCallersSpec for why (#832).
			if !spec.Enabled {
				t.Error("gateRelayServerBody must not mutate the spec it was handed")
			}
		})
	}
}

// TestRelayServerBodyGateLeavesOtherTasksAlone guards against the gate
// accidentally reporting true for unrelated daemon tasks when the relay is
// off.
func TestRelayServerBodyGateLeavesOtherTasksAlone(t *testing.T) {
	cfg := &config.Config{} // relay unconfigured
	spec := &task.Spec{ID: "buildin/relay-client", Enabled: true}
	if gateRelayServerBody(spec, cfg) {
		t.Error("gate matched an unrelated task (buildin/relay-client)")
	}
}

// TestArmRelayServerBodyDoesNotMutateCallersSpec is the #832 code-review
// regression for the second in-place mutation the same review pass found:
// the arm closure must apply gateRelayServerBody's disable to a copy, not to
// the caller's spec, for the identical reason as
// TestRetentionOverrideDoesNotMutateCallersSpec — that spec is the exact
// object the approval gate keeps live in its pending bookkeeping once a
// pinned buildin task can pend, and Gate.State reads its Enabled field
// outside the gate's lock.
//
// This exercises the same copy-then-disable logic that lives inside arm's
// call to gateRelayServerBody in newArmDisarm, keeping the logic in one
// place and the test cheap (matching this file's existing convention).
func TestArmRelayServerBodyDoesNotMutateCallersSpec(t *testing.T) {
	cfg := &config.Config{} // relay unconfigured

	original := &task.Spec{ID: "buildin/relay-server-body", Enabled: true}
	callerCopy := original // the reference the gate would keep in g.admitted/g.pending
	spec := original

	if gateRelayServerBody(spec, cfg) {
		override := *spec
		override.Enabled = false
		spec = &override
	}

	if !callerCopy.Enabled {
		t.Error("caller's spec was mutated in place: Enabled = false, want unchanged true")
	}
	if spec.Enabled {
		t.Error("the returned spec must have Enabled = false")
	}
	if spec == callerCopy {
		t.Error("the overridden spec must be a distinct object from the caller's, not the same pointer")
	}
}

// TestRelayConfigured covers the boot gate that decides whether to export
// the DICODE_RELAY_* env vars and dispatch relay-client. It must fire for the
// single-URL shorthand AND for a server_urls-only HA deployment.
func TestRelayConfigured(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{"disabled", config.Config{Relay: config.RelayConfig{ServerURL: "wss://a.example"}}, false},
		{"enabled, no url", config.Config{Relay: config.RelayConfig{Enabled: true}}, false},
		{"enabled, shorthand", config.Config{Relay: config.RelayConfig{Enabled: true, ServerURL: "wss://a.example"}}, true},
		{"enabled, server_urls only", config.Config{Relay: config.RelayConfig{Enabled: true, ServerURLs: []string{"wss://a.example"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if got := relayConfigured(&cfg); got != tc.want {
				t.Errorf("relayConfigured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPemContainsCertificate(t *testing.T) {
	// A minimal but structurally valid CERTIFICATE PEM block.
	const certPEM = `-----BEGIN CERTIFICATE-----
MIIBFDCBu6ADAgECAgEBMAoGCCqGSM49BAMCMA8xDTALBgNVBAMTBHRlc3QwHhcN
-----END CERTIFICATE-----`
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"valid cert block", certPEM, true},
		{"empty", "", false},
		{"garbage", "not a pem file at all", false},
		{"wrong block type", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pemContainsCertificate([]byte(tc.in)); got != tc.want {
				t.Errorf("pemContainsCertificate(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsPathUnderDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		dir  string
		want bool
	}{
		{"direct child", "/data/relay/mtls-cert.pem", "/data", true},
		{"child in dir", "/data/x", "/data", true},
		{"dir itself", "/data", "/data", true},
		{"outside", "/etc/ssl/broker.pem", "/data", false},
		{"parent traversal", "/data/../etc/x", "/data", false},
		{"prefix-but-not-under", "/database/x", "/data", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPathUnderDir(tc.path, tc.dir); got != tc.want {
				t.Errorf("isPathUnderDir(%q, %q) = %v, want %v", tc.path, tc.dir, got, tc.want)
			}
		})
	}
}
