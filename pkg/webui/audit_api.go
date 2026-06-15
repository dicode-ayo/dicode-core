package webui

import (
	"net/http"
	"strconv"

	"github.com/dicode/dicode/pkg/audit"
	"go.uber.org/zap"
)

// apiQueryAudit serves
// GET /api/audit?task_id=&actor=&event_type=&limit=&offset=&after=&order=
// — the paginated, filterable view over the security audit log (#45).
// Registered inside the /api route group, so it sits behind requireAuth
// like every other API route.
//
// Default behaviour is unchanged: newest-first (order=desc), offset paging,
// limit 100 capped at 1000. A consumer that wants exactly-once incremental
// shipping passes order=asc plus the opaque after= cursor from a prior
// response's next_cursor; the cursor takes precedence over offset.
func (s *Server) apiQueryAudit(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		jsonErr(w, "audit log unavailable (no database)", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	ascending := false
	switch q.Get("order") {
	case "", "desc":
		// default — newest first
	case "asc":
		ascending = true
	default:
		jsonErr(w, "order must be asc or desc", http.StatusBadRequest)
		return
	}

	after, err := audit.DecodeCursor(q.Get("after"))
	if err != nil {
		jsonErr(w, "invalid after cursor", http.StatusBadRequest)
		return
	}

	events, err := s.audit.Query(r.Context(), audit.Filter{
		TaskID:    q.Get("task_id"),
		Actor:     q.Get("actor"),
		EventType: q.Get("event_type"),
		Limit:     limit,
		Offset:    offset,
		After:     after,
		Ascending: ascending,
	})
	if err != nil {
		s.log.Error("audit query failed", zap.Error(err))
		jsonErr(w, "audit query failed", http.StatusInternalServerError)
		return
	}
	// next_cursor is the position of the last row in this page; a consumer
	// resumes by passing it back as after=. Empty when the page is empty.
	var nextCursor string
	if n := len(events); n > 0 {
		nextCursor = audit.EncodeCursor(audit.CursorOf(events[n-1]))
	}
	jsonOK(w, map[string]any{
		"events":      events,
		"count":       len(events),
		"next_cursor": nextCursor,
	})
}
