package webui

import (
	"encoding/json"
	"net/http"

	"github.com/dicode/dicode/pkg/metrics"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
)

// apiMetrics handles GET /api/metrics.
// Returns daemon heap/CPU metrics and active task/child-process metrics as JSON.
func (s *Server) apiMetrics(w http.ResponseWriter, r *http.Request) {
	activeTasks := s.engine.ActiveRunCount()
	pids := denoruntime.ActivePIDs()

	m := metrics.Metrics{
		Daemon: metrics.ReadDaemonMetrics(),
		Tasks:  metrics.ReadChildMetrics(pids, activeTasks),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(m)
}
