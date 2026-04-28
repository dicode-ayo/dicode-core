package deno

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap/zaptest"
)

// newTestRuntime builds a Deno Runtime backed by an in-memory SQLite db,
// suitable for integration tests that exercise the IPC server's
// secret-output path (issue #119). The cleanup func closes the db; the
// per-test t.TempDir already auto-cleans on test exit.
//
// Returns t.Skip if the Deno binary cannot be located/downloaded so the
// test exits cleanly on hosts without network access to deno.land.
func newTestRuntime(t *testing.T) (*Runtime, *registry.Registry, func()) {
	t.Helper()
	tdb, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	reg := registry.New(tdb)
	rt, err := New(reg, secrets.Chain{}, tdb, zaptest.NewLogger(t))
	if err != nil {
		_ = tdb.Close()
		t.Skipf("deno not available: %v", err)
	}
	return rt, reg, func() { _ = tdb.Close() }
}

// writeProviderTask materialises a minimal provider-style Deno task on
// disk (task.yaml + task.ts) and returns the parsed Spec. The task is
// declared with manual triggering and a 5m provider cache TTL — enough
// shape for the secret-output flow without pulling in scheduler state.
//
// Used by the Bundle F integration tests for issue #119.
func writeProviderTask(t *testing.T, id, body string) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "task.yaml")
	tsPath := filepath.Join(dir, "task.ts")
	yamlContent := `apiVersion: dicode/v1
kind: Task
name: ` + id + `
runtime: deno
trigger:
  manual: true
provider:
  cache_ttl: 5m
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tsPath, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	spec, err := task.LoadDir(dir)
	if err != nil {
		t.Fatalf("load provider task spec: %v", err)
	}
	spec.ID = id
	return spec
}
