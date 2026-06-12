package webui

import (
	"net/http"
	"strconv"

	"github.com/dicode/dicode/pkg/audit"
)

// apiQueryAudit serves GET /api/audit?task_id=&actor=&event_type=&limit=&offset=
// — the paginated, filterable view over the security audit log (#45).
// Registered inside the /api route group, so it sits behind requireAuth
// like every other API route. Events are returned newest first; limit
// defaults to 100 and is capped by the store.
func (s *Server) apiQueryAudit(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		jsonErr(w, "audit log unavailable (no database)", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	events, err := s.audit.Query(r.Context(), audit.Filter{
		TaskID:    q.Get("task_id"),
		Actor:     q.Get("actor"),
		EventType: q.Get("event_type"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		s.log.Error("audit query failed")
		jsonErr(w, "audit query failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{
		"events": events,
		"count":  len(events),
	})
}
