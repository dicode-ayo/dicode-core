package runinput

import (
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
)

// newTestRegistry returns a registry over an in-memory database, closed when
// the test ends. Replay reads real run rows, so these tests drive the run log
// rather than a stand-in for it.
func newTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return registry.New(d)
}
