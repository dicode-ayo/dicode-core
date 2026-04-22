package relay

import (
	"context"
	"net/http"
	"testing"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

func noopLogger() *zap.Logger {
	return zap.NewNop()
}

func dialWS(ctx context.Context, url string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, url, nil)
}

// newTestIdentity generates a fresh identity backed by an in-memory SQLite DB.
// Multiple tests across the package need one; keeping the helper here avoids
// a test-file-to-test-file dependency.
func newTestIdentity(t *testing.T) *Identity {
	t.Helper()
	ctx := context.Background()
	database := openTestDB(t)
	id, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}
