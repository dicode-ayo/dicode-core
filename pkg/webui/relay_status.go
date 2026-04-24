package webui

import (
	"encoding/json"
	"net/http"
)

// apiRelayStatus serves GET /api/relay/status. When the daemon was
// started without a relay client (relay disabled in config), the
// response is simply {"enabled":false} so the frontend can hide the
// header badge.
func (s *Server) apiRelayStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.relayClient == nil {
		_ = json.NewEncoder(w).Encode(struct {
			Enabled bool `json:"enabled"`
		}{Enabled: false})
		return
	}
	_ = json.NewEncoder(w).Encode(s.relayClient.Status())
}
