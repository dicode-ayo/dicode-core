package relay

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

func noopLogger() *zap.Logger {
	return zap.NewNop()
}

func dialWS(ctx context.Context, url string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, url, nil)
}
