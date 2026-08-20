package daemon

import (
	"context"
	"reflect"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/secrets"

	"go.uber.org/zap"
)

// optionalBridgeDeps names the BridgeDeps fields a runtime may legitimately
// leave nil when buildRuntimes returns: each one is either wired later in the
// boot sequence (initSources, wireRunInputPersistence, setupApprovalGate,
// wireMCPTokenMinter) or gates an opt-in capability that stays inert when
// unset. Every other field is a hard dependency of the socket bridge, so a
// new field is treated as required until it is listed here.
var optionalBridgeDeps = map[string]bool{
	"InputStore":     true, // wireRunInputPersistence, only when a SubKeyDeriver exists
	"SecretOutputCh": true, // swapped in per provider invocation by the trigger engine
	"Replayer":       true, // wireRunInputPersistence
	"SourceMgr":      true, // initSources
	"RepoResolver":   true, // initSources
	"TestGuard":      true, // setupApprovalGate
	"MCPTokenMinter": true, // wireMCPTokenMinter
	"ProtectedPaths": true, // setupApprovalGate
}

// TestBuildRuntimesWiresBothRuntimes guards the wiring obligation BridgeDeps
// does not enforce by construction: buildRuntimes wires each dependency twice,
// once per socket-bridge runtime, and a setter called for only one of them
// leaves that runtime's capability dead at boot with no compile-time signal.
// Dependencies wired outside buildRuntimes are late-wired and exempt, so the
// covered set is every field not named above.
func TestBuildRuntimesWiresBothRuntimes(t *testing.T) {
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	reg := registry.New(database)
	_, _, denoRT, pythonRT, err := buildRuntimes(
		context.Background(), &config.Config{}, reg, secrets.Chain{}, stubSecretsManager{},
		database, zap.NewNop(), ipc.NewGateway(),
	)
	if err != nil {
		t.Fatalf("buildRuntimes: %v", err)
	}

	for _, rt := range []struct {
		name string
		deps *pkgruntime.BridgeDeps
	}{
		{"deno", &denoRT.BridgeDeps},
		{"python", &pythonRT.BridgeDeps},
	} {
		t.Run(rt.name, func(t *testing.T) {
			v := reflect.ValueOf(*rt.deps)
			for i := 0; i < v.NumField(); i++ {
				name := v.Type().Field(i).Name
				if optionalBridgeDeps[name] {
					continue
				}
				if isNilValue(v.Field(i)) {
					t.Errorf("BridgeDeps.%s is nil after buildRuntimes: the %s runtime is missing its setter call", name, rt.name)
				}
			}
		})
	}
}

// isNilValue reports whether a BridgeDeps field is unset. Only nilable kinds
// can be unwired; anything else is reported as set.
func isNilValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// snapshotDeps are dependencies NewIPCServer reads from its receiver — the copy
// NewExecutor took — rather than from the live manager, and that buildRuntimes
// must therefore have set before it constructs an executor. A dependency wired
// onto the manager afterwards is nil for every run that executor serves.
var snapshotDeps = []string{"Registry", "DB", "Log", "IPCSecret", "Engine", "Gateway", "SecretsManager"}

// TestExecutorSnapshotsDeps holds each socket-bridge runtime's NewExecutor
// copy list to the set NewIPCServer reads from its receiver: a per-version
// executor (built when runtimes.<name>.version pins an installed version)
// serves runs from its own snapshot, so a dependency the manager holds but
// NewExecutor omits is nil for every run that executor serves.
//
// Regression coverage for issue #718: before the fix, the Deno subtest's
// SecretsManager/IPCSecret/Engine/Gateway checks failed — Deno's NewExecutor
// copied only six of the ten construction-time BridgeDeps fields, so a
// pinned Deno version's dicode.run_task failed with "engine not available",
// dicode.http/secrets_set/secrets_delete were inert, and — the
// security-relevant one — per-run IPC capability tokens were minted and
// verified under a nil IPCSecret (an implicit all-zero HMAC key) rather than
// the daemon's real secret. The Python subtest passed before the fix and
// guards against the same class recurring there.
func TestExecutorSnapshotsDeps(t *testing.T) {
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	_, _, denoRT, pythonRT, err := buildRuntimes(
		context.Background(), &config.Config{}, registry.New(database), secrets.Chain{},
		stubSecretsManager{}, database, zap.NewNop(), ipc.NewGateway(),
	)
	if err != nil {
		t.Fatalf("buildRuntimes: %v", err)
	}

	for _, rt := range []struct {
		name       string
		newExecFn  func() pkgruntime.Executor
		binaryPath string
	}{
		{"deno", func() pkgruntime.Executor { return denoRT.NewExecutor("/nonexistent/deno") }, "/nonexistent/deno"},
		{"python", func() pkgruntime.Executor { return pythonRT.NewExecutor("/nonexistent/uv") }, "/nonexistent/uv"},
	} {
		t.Run(rt.name, func(t *testing.T) {
			deps := reflect.ValueOf(rt.newExecFn()).Elem().FieldByName("BridgeDeps")
			for _, name := range snapshotDeps {
				f := deps.FieldByName(name)
				if !f.IsValid() {
					t.Fatalf("BridgeDeps has no field %q: renaming a field must not silently drop its coverage", name)
				}
				if isNilValue(f) {
					t.Errorf("executor BridgeDeps.%s is nil: NewExecutor does not copy it, so no run it serves sees it", name)
				}
			}
		})
	}
}

// TestDenoExecutorHasRealIPCSecret is the focused regression test the issue
// asks for: a per-version Deno executor's IPCSecret must be the daemon's
// actual, non-empty secret — not merely non-nil (a nil []byte and an empty
// []byte both fail IsNil, but only a byte slice of real length signs tokens
// under a real key; see TestIssueTokenRejectsEmptySecret in pkg/ipc for the
// other half of this guarantee).
func TestDenoExecutorHasRealIPCSecret(t *testing.T) {
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	_, _, denoRT, _, err := buildRuntimes(
		context.Background(), &config.Config{}, registry.New(database), secrets.Chain{},
		stubSecretsManager{}, database, zap.NewNop(), ipc.NewGateway(),
	)
	if err != nil {
		t.Fatalf("buildRuntimes: %v", err)
	}

	wantSecret := denoRT.BridgeDeps.IPCSecret
	if len(wantSecret) == 0 {
		t.Fatal("manager's own IPCSecret is empty; nothing meaningful to propagate")
	}

	exec := denoRT.NewExecutor("/nonexistent/deno")
	deps := reflect.ValueOf(exec).Elem().FieldByName("BridgeDeps")
	got := deps.FieldByName("IPCSecret").Bytes()
	if len(got) == 0 {
		t.Fatal("per-version Deno executor's IPCSecret is empty: per-run IPC tokens would sign/verify under an implicit all-zero key")
	}
	if string(got) != string(wantSecret) {
		t.Errorf("per-version Deno executor's IPCSecret does not match the manager's: got %x, want %x", got, wantSecret)
	}
}

type stubSecretsManager struct{}

func (stubSecretsManager) List(context.Context) ([]string, error)    { return nil, nil }
func (stubSecretsManager) Has(context.Context, string) (bool, error) { return false, nil }
func (stubSecretsManager) Set(_ context.Context, _, _ string) error  { return nil }
func (stubSecretsManager) Delete(context.Context, string) error      { return nil }
