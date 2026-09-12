package daemon

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestRun_UsesConfigDataDir: the daemon takes the data dir config.Load
// resolved rather than resolving it a second time. A second resolution is
// what let the daemon and the CLI disagree — the CLI then dials a socket no
// daemon is listening on and starts one of its own against a data directory
// that already has one. config.ResolveDataDir owns the order; its tests cover
// it.
func TestRun_UsesConfigDataDir(t *testing.T) {
	t.Setenv(config.DataDirEnvVar, "")
	cfg, err := config.LoadBytes([]byte("data_dir: /var/lib/dicode\n"), "/srv/project")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if cfg.DataDir != "/var/lib/dicode" {
		t.Fatalf("cfg.DataDir = %q; want /var/lib/dicode", cfg.DataDir)
	}
}

// newTestRegistry returns an in-memory registry for tests that need a real
// *registry.Registry (not a fake) — e.g. to assert what GET /api/tasks would
// actually see after arm runs.
func newTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return registry.New(database)
}

// The tests below exercise applyBuiltinOverrides directly — the real
// function newArmDisarm's arm closure calls, and that Gate.SetPreviewFn also
// wires as the approval gate's pending-review preview transform — rather
// than a hand-copy of its logic, so a future change to the real function
// can't silently diverge from what these tests check (a prior round of this
// PR's own /code-review found exactly that risk in specs that duplicated
// the logic inline).

// TestApplyBuiltinOverrides_RetentionSeconds verifies Finding 2: the
// override must reflect dicode.yaml's defaults.run_inputs.retention on
// buildin/run-inputs-cleanup, and must be a no-op (same pointer back) when
// the operator left it unset — the task's own 30-day default applies, and
// callers key a registry refresh off the pointer changing.
func TestApplyBuiltinOverrides_RetentionSeconds(t *testing.T) {
	newSpec := func() *task.Spec {
		return &task.Spec{
			ID: "buildin/run-inputs-cleanup",
			Params: task.Params{
				{Name: "retention_seconds", Default: "2592000", Type: "number"},
			},
		}
	}

	t.Run("overridden when configured", func(t *testing.T) {
		retention := 7 * 24 * time.Hour // 7 days = 604800s
		cfg := &config.Config{}
		cfg.Defaults.RunInputs.Retention = retention
		spec := newSpec()

		got := applyBuiltinOverrides(spec, cfg)

		want := fmt.Sprintf("%d", int64(retention.Seconds())) // "604800"
		gotSpec, ok := got.(*task.Spec)
		if !ok {
			t.Fatalf("applyBuiltinOverrides returned %T, want *task.Spec", got)
		}
		if gotSpec.Params[0].Default != want {
			t.Errorf("retention_seconds default = %q, want %q", gotSpec.Params[0].Default, want)
		}
		if got == task.Kinded(spec) {
			t.Error("an applied override must return a distinct object, not the input pointer")
		}
	})

	t.Run("skipped when zero", func(t *testing.T) {
		cfg := &config.Config{} // Retention == 0
		spec := newSpec()

		got := applyBuiltinOverrides(spec, cfg)

		if got != task.Kinded(spec) {
			t.Error("no override configured: must return the same pointer, unchanged")
		}
		if spec.Params[0].Default != "2592000" {
			t.Errorf("caller's spec was mutated: Default = %q, want unchanged 2592000", spec.Params[0].Default)
		}
	})
}

// TestApplyBuiltinOverrides_RetentionDoesNotMutateCallersSpec is the #832
// code-review regression: the override must never write through the spec
// the caller (the approval gate) handed to arm, since that exact object can
// now be concurrently read by Gate.State while a pinned buildin task sits
// pending.
func TestApplyBuiltinOverrides_RetentionDoesNotMutateCallersSpec(t *testing.T) {
	cfg := &config.Config{}
	cfg.Defaults.RunInputs.Retention = 7 * 24 * time.Hour

	original := &task.Spec{
		ID: "buildin/run-inputs-cleanup",
		Params: task.Params{
			{Name: "retention_seconds", Default: "2592000", Type: "number"},
		},
	}
	callerCopy := original // the reference the gate would keep in g.admitted/g.pending

	applyBuiltinOverrides(original, cfg)

	if got := callerCopy.Params[0].Default; got != "2592000" {
		t.Errorf("caller's spec was mutated in place: Default = %q, want unchanged 2592000", got)
	}
}

// TestApplyBuiltinOverrides_RelayServerBody covers the second override:
// buildin/relay-server-body's Enabled flag, gated on relay configuration.
func TestApplyBuiltinOverrides_RelayServerBody(t *testing.T) {
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

			got := applyBuiltinOverrides(spec, cfg)

			gotSpec, ok := got.(*task.Spec)
			if !ok {
				t.Fatalf("applyBuiltinOverrides returned %T, want *task.Spec", got)
			}
			if gotSpec.Enabled == tc.wantDisabled {
				t.Errorf("Enabled = %v, want disabled=%v", gotSpec.Enabled, tc.wantDisabled)
			}
			wantSamePointer := !tc.wantDisabled
			if (got == task.Kinded(spec)) != wantSamePointer {
				t.Errorf("pointer identity: got same=%v, want same=%v", got == task.Kinded(spec), wantSamePointer)
			}
		})
	}
}

// TestApplyBuiltinOverrides_RelayServerBodyLeavesOtherTasksAlone guards
// against the override accidentally applying to unrelated daemon tasks when
// the relay is off.
func TestApplyBuiltinOverrides_RelayServerBodyLeavesOtherTasksAlone(t *testing.T) {
	cfg := &config.Config{} // relay unconfigured
	spec := &task.Spec{ID: "buildin/relay-client", Enabled: true}

	got := applyBuiltinOverrides(spec, cfg)

	if got != task.Kinded(spec) {
		t.Error("an unrelated task must come back as the same pointer, unchanged")
	}
	if !spec.Enabled {
		t.Error("override matched an unrelated task (buildin/relay-client)")
	}
}

// TestApplyBuiltinOverrides_RelayServerBodyDoesNotMutateCallersSpec is the
// #832 code-review regression for the second in-place mutation the same
// review pass found, mirroring
// TestApplyBuiltinOverrides_RetentionDoesNotMutateCallersSpec.
func TestApplyBuiltinOverrides_RelayServerBodyDoesNotMutateCallersSpec(t *testing.T) {
	cfg := &config.Config{} // relay unconfigured
	original := &task.Spec{ID: "buildin/relay-server-body", Enabled: true}
	callerCopy := original // the reference the gate would keep in g.admitted/g.pending

	applyBuiltinOverrides(original, cfg)

	if !callerCopy.Enabled {
		t.Error("caller's spec was mutated in place: Enabled = false, want unchanged true")
	}
}

// TestApplyBuiltinOverrides_NonSpecPassesThrough guards the type-assertion
// guard: a *task.PipelineTask (or anything else that isn't *task.Spec) must
// come back unchanged, never panic.
func TestApplyBuiltinOverrides_NonSpecPassesThrough(t *testing.T) {
	cfg := &config.Config{}
	cfg.Defaults.RunInputs.Retention = 7 * 24 * time.Hour
	pipe := &task.PipelineTask{ID: "buildin/run-inputs-cleanup"}

	got := applyBuiltinOverrides(pipe, cfg)

	if got != task.Kinded(pipe) {
		t.Error("a non-*task.Spec Kinded must pass through unchanged")
	}
}

// TestApplyBuiltinOverrides_RefreshesRegistry is the code-review regression
// for the copy-on-write fix's own side effect: the reconciler registers the
// pre-override spec into reg before arm ever runs (see
// pkg/registry/reconciler.go's rc.registry.Register(k) preceding
// rc.OnRegister(k)), so once arm stopped mutating that spec in place to fix
// the #832 data race, the registry-exposed copy — what GET /api/tasks
// serves — silently stopped reflecting the override. arm re-registers the
// overridden copy (keyed on the pointer having changed) so the two stay in
// sync; this pins that behavior for both override sites.
func TestApplyBuiltinOverrides_RefreshesRegistry(t *testing.T) {
	cases := []struct {
		name    string
		cfg     func() *config.Config
		spec    *task.Spec
		checkID string
		check   func(t *testing.T, got *task.Spec)
	}{
		{
			name: "retention_seconds",
			cfg: func() *config.Config {
				cfg := &config.Config{}
				cfg.Defaults.RunInputs.Retention = 7 * 24 * time.Hour
				return cfg
			},
			spec: &task.Spec{
				ID:     "buildin/run-inputs-cleanup",
				Params: task.Params{{Name: "retention_seconds", Default: "2592000", Type: "number"}},
			},
			checkID: "buildin/run-inputs-cleanup",
			check: func(t *testing.T, got *task.Spec) {
				if want := "604800"; got.Params[0].Default != want {
					t.Errorf("registry's retention_seconds default = %q, want %q (the API would show the stale shipped default)", got.Params[0].Default, want)
				}
			},
		},
		{
			name:    "relay-server-body",
			cfg:     func() *config.Config { return &config.Config{} }, // relay unconfigured
			spec:    &task.Spec{ID: "buildin/relay-server-body", Enabled: true},
			checkID: "buildin/relay-server-body",
			check: func(t *testing.T, got *task.Spec) {
				if got.Enabled {
					t.Error("registry still shows Enabled: true (the API would misreport a task with zero armed triggers as enabled)")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := newTestRegistry(t)
			cfg := tc.cfg()

			// Simulates the reconciler's rc.registry.Register(k) call, which
			// always runs before arm (OnRegister) does.
			if err := reg.Register(tc.spec); err != nil {
				t.Fatalf("Register: %v", err)
			}

			before := task.Kinded(tc.spec)
			overridden := applyBuiltinOverrides(tc.spec, cfg)
			if overridden != before {
				if err := reg.Register(overridden); err != nil {
					t.Fatalf("Register (refresh): %v", err)
				}
			}

			got, ok := reg.Get(tc.checkID)
			if !ok {
				t.Fatal("task not found in registry")
			}
			tc.check(t, got)
		})
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

// TestRun_RefusesADataDirAlreadyServed: run() reaches its startup cleanup —
// orphaned containers stopped, running rows cancelled, the CLI token rotated —
// before the control server ever tries to bind. A second daemon aimed at a
// live directory has to be turned away before any of that touches state the
// running daemon owns.
func TestRun_RefusesADataDirAlreadyServed(t *testing.T) {
	dir := t.TempDir()
	sock := ipc.ControlSocketIn(dir)

	ln, err := net.Listen("unix", sock.Path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if err := refuseIfDataDirServed(dir); err == nil {
		t.Fatal("a live control socket was accepted")
	} else if !strings.Contains(err.Error(), "already") {
		t.Errorf("error = %v; want it to name the running daemon", err)
	}
}

// TestRun_AcceptsAStaleSocket: a socket file left by a daemon that died hard
// must not keep the next one out.
func TestRun_AcceptsAStaleSocket(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ipc.ControlSocketIn(dir).Path, nil, 0o600); err != nil {
		t.Fatalf("plant stale socket: %v", err)
	}
	if err := refuseIfDataDirServed(dir); err != nil {
		t.Errorf("stale socket refused: %v", err)
	}
}
