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

// severAllWS closes every active WebSocket connection currently tracked by
// the Server. Used by reconnect tests — httptest.Server.CloseClientConnections
// is a no-op on hijacked (upgraded) connections, so it does NOT force a
// daemon-side reconnect on its own. Reaching into the server's client map
// and closing each WS forces the daemon's read loop to return and runOnce
// to exit, which is what triggers Run's reconnect cycle.
func severAllWS(s *Server) {
	s.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.clients))
	for _, sc := range s.clients {
		conns = append(conns, sc.conn)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow()
	}
}
