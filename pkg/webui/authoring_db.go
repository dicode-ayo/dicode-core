package webui

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

// AuthoringSession represents an AI-first task authoring session.
type AuthoringSession struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`         // "create" or "edit"
	Source      string    `json:"source"`       // source name (e.g. "ai-scratch")
	TaskID      string    `json:"task_id"`      // empty for new-task sessions until scaffold
	SandboxPath string    `json:"sandbox_path"` // working directory for the session
	CreatedAt   time.Time `json:"created_at"`
	LastTurnAt  time.Time `json:"last_turn_at"`
	ClosedAt    *int64    `json:"closed_at"` // nil = open
	Applied     bool      `json:"applied"`
	// AgentSessionID is the underlying ai-agent conversation's own session
	// id (#568). Nil until the session's first AI turn completes; set by
	// UpdateAgentSessionID afterward so the NEXT `dicode task edit` on the
	// same authoring session continues that conversation instead of
	// starting a new one. A plain (non-AI) edit session never sets it.
	AgentSessionID *string `json:"agent_session_id,omitempty"`
}

// authoringSessionStore wraps db.DB to manage author_sessions rows.
type authoringSessionStore struct {
	db db.DB
}

func newAuthoringSessionStore(d db.DB) *authoringSessionStore {
	return &authoringSessionStore{db: d}
}

// Create inserts a new authoring session.
func (s *authoringSessionStore) Create(ctx context.Context, sess AuthoringSession) error {
	applied := 0
	if sess.Applied {
		applied = 1
	}
	return s.db.Exec(ctx,
		`INSERT INTO author_sessions (id, kind, source, task_id, sandbox_path, created_at, last_turn_at, closed_at, applied, agent_session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Kind, sess.Source, sess.TaskID, sess.SandboxPath,
		sess.CreatedAt.Unix(), sess.LastTurnAt.Unix(), sess.ClosedAt, applied, sess.AgentSessionID,
	)
}

// Get returns an authoring session by ID.
func (s *authoringSessionStore) Get(ctx context.Context, id string) (*AuthoringSession, error) {
	var sess AuthoringSession
	var found bool
	err := s.db.Query(ctx,
		`SELECT id, kind, source, task_id, sandbox_path, created_at, last_turn_at, closed_at, applied, agent_session_id
		 FROM author_sessions WHERE id = ?`,
		[]any{id},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return scanAuthoringSession(rows, &sess)
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
	return &sess, nil
}

// GetOpenForSource returns the single open session for a given source, or nil.
func (s *authoringSessionStore) GetOpenForSource(ctx context.Context, source string) (*AuthoringSession, error) {
	var sess AuthoringSession
	var found bool
	err := s.db.Query(ctx,
		`SELECT id, kind, source, task_id, sandbox_path, created_at, last_turn_at, closed_at, applied, agent_session_id
		 FROM author_sessions WHERE source = ? AND closed_at IS NULL
		 LIMIT 1`,
		[]any{source},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return scanAuthoringSession(rows, &sess)
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
	return &sess, nil
}

// UpdateLastTurn bumps the last_turn_at timestamp for the given session.
func (s *authoringSessionStore) UpdateLastTurn(ctx context.Context, id string) error {
	return s.db.Exec(ctx,
		`UPDATE author_sessions SET last_turn_at = ? WHERE id = ?`,
		time.Now().Unix(), id,
	)
}

// UpdateAgentSessionID persists the underlying ai-agent conversation's own
// session id onto the authoring session row (#568), so the next `dicode task
// edit` call against this session can read it back and continue the same
// conversation instead of starting a fresh one. Called after a successful AI
// turn; a blank agentSessionID is a no-op write (some alternative agent
// tasks may not return one) rather than clobbering a previously stored id.
func (s *authoringSessionStore) UpdateAgentSessionID(ctx context.Context, id, agentSessionID string) error {
	if agentSessionID == "" {
		return nil
	}
	return s.db.Exec(ctx,
		`UPDATE author_sessions SET agent_session_id = ? WHERE id = ?`,
		agentSessionID, id,
	)
}

// Close marks a session as closed, setting closed_at and the applied flag.
func (s *authoringSessionStore) Close(ctx context.Context, id string, applied bool) error {
	appliedInt := 0
	if applied {
		appliedInt = 1
	}
	return s.db.Exec(ctx,
		`UPDATE author_sessions SET closed_at = ?, applied = ? WHERE id = ?`,
		time.Now().Unix(), appliedInt, id,
	)
}

// ListOpen returns all sessions that have not been closed.
func (s *authoringSessionStore) ListOpen(ctx context.Context) ([]AuthoringSession, error) {
	var sessions []AuthoringSession
	err := s.db.Query(ctx,
		`SELECT id, kind, source, task_id, sandbox_path, created_at, last_turn_at, closed_at, applied, agent_session_id
		 FROM author_sessions WHERE closed_at IS NULL
		 ORDER BY created_at DESC`,
		nil,
		func(rows db.Scanner) error {
			for rows.Next() {
				var sess AuthoringSession
				if err := scanAuthoringSession(rows, &sess); err != nil {
					return err
				}
				sessions = append(sessions, sess)
			}
			return nil
		},
	)
	return sessions, err
}

// PurgeExpired closes sessions whose last_turn_at is older than ttl ago.
// Returns the number of sessions closed.
func (s *authoringSessionStore) PurgeExpired(ctx context.Context, ttl time.Duration) (int, error) {
	cutoff := time.Now().Add(-ttl).Unix()
	now := time.Now().Unix()

	// Count how many will be purged.
	var count int
	err := s.db.Query(ctx,
		`SELECT COUNT(*) FROM author_sessions WHERE closed_at IS NULL AND last_turn_at < ?`,
		[]any{cutoff},
		func(rows db.Scanner) error {
			if rows.Next() {
				return rows.Scan(&count)
			}
			return nil
		},
	)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	err = s.db.Exec(ctx,
		`UPDATE author_sessions SET closed_at = ?, applied = 0 WHERE closed_at IS NULL AND last_turn_at < ?`,
		now, cutoff,
	)
	return count, err
}

// scanAuthoringSession scans a row into an AuthoringSession.
func scanAuthoringSession(rows db.Scanner, sess *AuthoringSession) error {
	var createdAt, lastTurnAt int64
	var closedAt sql.NullInt64
	var applied int
	var agentSessionID sql.NullString
	if err := rows.Scan(
		&sess.ID, &sess.Kind, &sess.Source, &sess.TaskID, &sess.SandboxPath,
		&createdAt, &lastTurnAt, &closedAt, &applied, &agentSessionID,
	); err != nil {
		return err
	}
	sess.CreatedAt = time.Unix(createdAt, 0)
	sess.LastTurnAt = time.Unix(lastTurnAt, 0)
	if closedAt.Valid {
		sess.ClosedAt = &closedAt.Int64
	}
	sess.Applied = applied != 0
	if agentSessionID.Valid {
		sess.AgentSessionID = &agentSessionID.String
	}
	return nil
}

// errSessionNotFound is a sentinel for session lookup misses in handlers.
var errSessionNotFound = errors.New("authoring session not found")
