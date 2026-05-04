package webui

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/dicode/dicode/pkg/db"
)

// apiRelayStatus serves GET /api/relay/status. The status is published
// by the buildin/relay-client task via dicode.kv.set("status", ...). The
// kv layer namespaces by task ID, so we read the row at
// "buildin/relay-client:status" via direct SQL.
//
// Returns {"enabled":false} if no status row exists yet (e.g., relay
// disabled in config, or the task hasn't completed its first connect).
func (s *Server) apiRelayStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	raw, err := readKvJSON(r.Context(), s.db, "buildin/relay-client:status")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if raw == nil {
		_ = json.NewEncoder(w).Encode(struct {
			Enabled bool `json:"enabled"`
		}{Enabled: false})
		return
	}
	// Pass through the JSON payload verbatim — the schema is owned by
	// the relay-client task (RelayStatus from npm:dicode-relay/client).
	_, _ = w.Write(raw)
}

// readKvJSON reads a single JSON value from the kv table by key.
// Returns nil, nil if the key does not exist.
func readKvJSON(ctx context.Context, database db.DB, key string) (json.RawMessage, error) {
	if database == nil {
		return nil, nil
	}
	var raw string
	var found bool
	err := database.Query(ctx, `SELECT value FROM kv WHERE key = ?`,
		[]any{key},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return rows.Scan(&raw)
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}
