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
// Setters called outside buildRuntimes are late-wired and exempt, so the
// fields this actually covers are the ones listed as required below.
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

// snapshotDeps are the BridgeDeps fields NewIPCServer reads from its receiver
// — the snapshot NewExecutor took — rather than from the live manager. A
// dependency wired after NewExecutor never reaches a run through them, so the
// manager holding it is not enough.
var snapshotDeps = []string{"Registry", "DB", "Log", "IPCSecret", "Engine", "Gateway", "SecretsManager"}

// TestPythonExecutorSnapshotsDeps holds NewExecutor's copy list to the set
// NewIPCServer reads from its receiver: a per-version executor serves runs from
// its own snapshot, so a dependency the manager holds but NewExecutor omits is
// nil for every run that executor serves.
func TestPythonExecutorSnapshotsDeps(t *testing.T) {
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	_, _, _, pythonRT, err := buildRuntimes(
		context.Background(), &config.Config{}, registry.New(database), secrets.Chain{},
		stubSecretsManager{}, database, zap.NewNop(), ipc.NewGateway(),
	)
	if err != nil {
		t.Fatalf("buildRuntimes: %v", err)
	}

	deps := reflect.ValueOf(pythonRT.NewExecutor("/nonexistent/uv")).Elem().FieldByName("BridgeDeps")
	for _, name := range snapshotDeps {
		if isNilValue(deps.FieldByName(name)) {
			t.Errorf("executor BridgeDeps.%s is nil: NewExecutor does not copy it, so no run it serves sees it", name)
		}
	}
}

type stubSecretsManager struct{}

func (stubSecretsManager) List(context.Context) ([]string, error)    { return nil, nil }
func (stubSecretsManager) Has(context.Context, string) (bool, error) { return false, nil }
func (stubSecretsManager) Set(_ context.Context, _, _ string) error  { return nil }
func (stubSecretsManager) Delete(context.Context, string) error      { return nil }
