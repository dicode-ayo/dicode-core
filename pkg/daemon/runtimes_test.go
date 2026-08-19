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
	"SecretsManager": true, // dicode.secrets_set/delete; nil when no local provider
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
// does not enforce by construction: daemon.go calls every setter twice, once
// per socket-bridge runtime, and a setter called for only one of them leaves
// that runtime's capability dead at boot with no compile-time signal.
func TestBuildRuntimesWiresBothRuntimes(t *testing.T) {
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	reg := registry.New(database)
	_, _, denoRT, pythonRT, err := buildRuntimes(
		context.Background(), &config.Config{}, reg, secrets.Chain{}, nil,
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
