package webui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The REST handlers in authoring.go and the control-socket IPC handlers in
// pkg/ipc both call these methods, so the create/edit/save/cancel business
// logic lives in exactly one place.

// authoringError carries an HTTP-style status alongside the message so the
// REST layer can map it back to a response code while the IPC layer surfaces
// the same wording. The 409 conflict path (single open session per source)
// carries its #283 reference in the message itself and must reach CLI users
// intact.
type authoringError struct {
	status int
	msg    string
}

func (e *authoringError) Error() string { return e.msg }

func authErr(status int, format string, a ...any) *authoringError {
	return &authoringError{status: status, msg: fmt.Sprintf(format, a...)}
}

// CreateTask scaffolds boilerplate task files into the named source and
// returns the new task id. An empty source defaults to "ai-scratch"; an
// empty name is generated.
func (s *Server) CreateTask(ctx context.Context, name, source string) (ipc.AuthoringCreateResult, error) {
	if source == "" {
		source = "ai-scratch"
	}
	if s.sourceMgr == nil {
		return ipc.AuthoringCreateResult{}, authErr(503, "source manager not available")
	}
	src, ok := s.sourceMgr.Get(source)
	if !ok {
		return ipc.AuthoringCreateResult{}, authErr(404, "source %q not found", source)
	}

	if name == "" {
		name = "new-task-" + uuid.New().String()[:8]
	}
	name = sanitizeTaskName(name)
	if name == "" {
		return ipc.AuthoringCreateResult{}, authErr(400, "invalid task name")
	}
	// The entry key is what makes the scaffolded directory resolvable, so a
	// name the taskset would refuse must fail before any file is written —
	// otherwise the directory lands, registration fails, and the existence
	// check below rejects every retry.
	if err := taskset.ValidateEntryName(name); err != nil {
		return ipc.AuthoringCreateResult{}, authErr(400, "invalid task name: %v", err)
	}

	// Scaffold beside the source's root taskset file, not at the repo root:
	// the entry added below points at "./<name>/task.yaml", which the resolver
	// reads relative to the taskset file's own directory.
	tasksetPath := src.RootTaskSetPath()
	if tasksetPath == "" {
		return ipc.AuthoringCreateResult{}, authErr(503, "source has no repo path; is it started?")
	}
	taskDir := filepath.Join(filepath.Dir(tasksetPath), name)
	if _, err := os.Stat(filepath.Join(taskDir, "task.yaml")); err == nil {
		return ipc.AuthoringCreateResult{}, authErr(409, "task %q already exists in source %q", name, source)
	}
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		return ipc.AuthoringCreateResult{}, authErr(500, "create task dir: %v", err)
	}

	taskYAML := fmt.Sprintf(`apiVersion: dicode/v1
kind: Task
name: %s
description: ""
runtime: deno
trigger:
  manual: true
timeout: 30s
`, name)
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(taskYAML), 0644); err != nil {
		return ipc.AuthoringCreateResult{}, authErr(500, "write task.yaml: %v", err)
	}

	// The SDK reaches task code through the handler's ctx argument, not a
	// global — boilerplate that reads a bare `dicode` throws on the first run.
	taskJS := `export default async function main({ dicode }) {
  console.log("Hello from " + dicode.task_id);
}
`
	if err := os.WriteFile(filepath.Join(taskDir, "task.js"), []byte(taskJS), 0644); err != nil {
		return ipc.AuthoringCreateResult{}, authErr(500, "write task.js: %v", err)
	}

	// Resolution walks spec.entries and never scans the source tree, so a
	// scaffolded directory stays invisible to the daemon until it is listed.
	if err := taskset.AddTaskEntry(tasksetPath, name); err != nil {
		// An unregistered scaffold is invisible to the resolver but still trips
		// the existence check above, so drop what we just wrote rather than
		// wedging the name against every retry. Remove (not RemoveAll) so a
		// directory holding anything else is left alone.
		os.Remove(filepath.Join(taskDir, "task.js"))
		os.Remove(filepath.Join(taskDir, "task.yaml"))
		os.Remove(taskDir)
		if errors.Is(err, taskset.ErrEntryConflict) {
			return ipc.AuthoringCreateResult{}, authErr(409, "task name %q is already bound to another path in source %q", name, source)
		}
		return ipc.AuthoringCreateResult{}, authErr(500, "register task in taskset: %v", err)
	}

	return ipc.AuthoringCreateResult{
		TaskID: source + "/" + name,
		Source: source,
		Files:  []string{"task.yaml", "task.js"},
	}, nil
}

// EditTask opens a new AI edit session for taskID, or resumes the session
// identified by sessionID. Exactly one of the two must be supplied. The
// single-open-session-per-source rule is enforced here: an edit for a
// source whose open session is the SAME task auto-resumes that session; an edit
// for a DIFFERENT task in that source returns a 409 conflict.
func (s *Server) EditTask(ctx context.Context, sessionID, taskID string) (ipc.AuthoringEditResult, error) {
	if s.authoringSessions == nil {
		return ipc.AuthoringEditResult{}, authErr(503, "authoring sessions not available")
	}

	if sessionID != "" {
		sess, err := s.authoringSessions.Get(ctx, sessionID)
		if err != nil {
			return ipc.AuthoringEditResult{}, authErr(500, "lookup session: %v", err)
		}
		if sess == nil {
			return ipc.AuthoringEditResult{}, authErr(404, "session not found")
		}
		if sess.ClosedAt != nil {
			return ipc.AuthoringEditResult{}, authErr(409, "session is closed")
		}
		if taskID != "" && sess.TaskID != taskID {
			return ipc.AuthoringEditResult{}, authErr(409, "session %s belongs to task %q, not %q", sess.ID, sess.TaskID, taskID)
		}
		_ = s.authoringSessions.UpdateLastTurn(ctx, sess.ID)
		return ipc.AuthoringEditResult{
			SessionID:      sess.ID,
			TaskID:         sess.TaskID,
			SandboxPath:    sess.SandboxPath,
			Source:         sess.Source,
			SourceKind:     s.resolveSourceKind(sess.Source),
			AgentSessionID: derefOrEmpty(sess.AgentSessionID),
		}, nil
	}

	if taskID == "" {
		return ipc.AuthoringEditResult{}, authErr(400, "task_id or session_id required")
	}

	source := taskID
	if idx := strings.Index(source, "/"); idx > 0 {
		source = source[:idx]
	}

	// Single-session-per-source: auto-resume an open session for this
	// source rather than rejecting, so the CLI's task-id-keyed edit verb
	// transparently continues the in-flight conversation. A session opened
	// against a different task in the same source surfaces as a conflict.
	existing, err := s.authoringSessions.GetOpenForSource(ctx, source)
	if err != nil {
		return ipc.AuthoringEditResult{}, authErr(500, "check open sessions: %v", err)
	}
	if existing != nil {
		return s.resumeOrConflict(ctx, existing, source, taskID)
	}

	sessID := uuid.New().String()
	now := time.Now()
	sess := AuthoringSession{
		ID:         sessID,
		Kind:       "edit",
		Source:     source,
		TaskID:     taskID,
		CreatedAt:  now,
		LastTurnAt: now,
	}
	if err := s.authoringSessions.Create(ctx, sess); err != nil {
		// The partial unique index idx_author_sessions_open_source rejects a
		// second open session for this source. A concurrent insert that lost
		// the race lands here: re-read the winner and return the same 409 the
		// non-racing path returns, not a 500.
		if isUniqueConstraint(err) {
			if winner, gerr := s.authoringSessions.GetOpenForSource(ctx, source); gerr == nil && winner != nil {
				return s.resumeOrConflict(ctx, winner, source, taskID)
			}
		}
		return ipc.AuthoringEditResult{}, authErr(500, "create session: %v", err)
	}

	return ipc.AuthoringEditResult{
		SessionID:   sessID,
		TaskID:      taskID,
		SandboxPath: sess.SandboxPath,
		Source:      source,
		SourceKind:  s.resolveSourceKind(source),
		// AgentSessionID is always empty here: this is a brand-new session,
		// there is no prior AI turn to have set it.
	}, nil
}

// resumeOrConflict resumes an existing open session for a source when it is the
// same task, or returns the 409 single-session conflict when the open session
// belongs to a different task.
func (s *Server) resumeOrConflict(ctx context.Context, existing *AuthoringSession, source, taskID string) (ipc.AuthoringEditResult, error) {
	if existing.TaskID != "" && existing.TaskID != taskID {
		return ipc.AuthoringEditResult{}, authErr(409, "source %q already has an open session %s for task %q (#283)", source, existing.ID, existing.TaskID)
	}
	_ = s.authoringSessions.UpdateLastTurn(ctx, existing.ID)
	return ipc.AuthoringEditResult{
		SessionID:      existing.ID,
		TaskID:         existing.TaskID,
		SandboxPath:    existing.SandboxPath,
		Source:         existing.Source,
		SourceKind:     s.resolveSourceKind(existing.Source),
		AgentSessionID: derefOrEmpty(existing.AgentSessionID),
	}, nil
}

// derefOrEmpty returns *p, or "" when p is nil. Used to flatten the
// nullable AuthoringSession.AgentSessionID onto the IPC wire shape, where a
// plain string with "" meaning "unset" is simpler for callers than a pointer.
func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// UpdateAgentSessionID persists the underlying ai-agent conversation's own
// session id onto the named authoring session (#568). Implements
// ipc.AuthoringService for pkg/ipc's handleTaskEdit, which calls this after
// a successful AI turn so the next `dicode task edit` on the same session
// continues the same conversation. See authoringSessionStore.UpdateAgentSessionID
// for the blank-is-noop semantics.
func (s *Server) UpdateAgentSessionID(ctx context.Context, sessionID, agentSessionID string) error {
	if s.authoringSessions == nil {
		return authErr(503, "authoring sessions not available")
	}
	return s.authoringSessions.UpdateAgentSessionID(ctx, sessionID, agentSessionID)
}

// isUniqueConstraint reports whether err is a SQLite UNIQUE-constraint
// violation. The db layer surfaces the driver error verbatim; modernc.org/sqlite
// renders these as "...UNIQUE constraint failed: ...".
func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// SaveTask applies the session's pending changes and closes it as applied.
func (s *Server) SaveTask(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return authErr(400, "session_id required")
	}
	if s.authoringSessions == nil {
		return authErr(503, "authoring sessions not available")
	}
	sess, err := s.authoringSessions.Get(ctx, sessionID)
	if err != nil {
		return authErr(500, "lookup session: %v", err)
	}
	if sess == nil {
		return authErr(404, "session not found")
	}
	if sess.ClosedAt != nil {
		return authErr(409, "session is already closed")
	}
	if err := s.authoringSessions.Close(ctx, sess.ID, true); err != nil {
		return authErr(500, "close session: %v", err)
	}
	return nil
}

// CancelTask discards the session. It is idempotent: cancelling an
// already-closed session is a no-op success.
func (s *Server) CancelTask(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return authErr(400, "session_id required")
	}
	if s.authoringSessions == nil {
		return authErr(503, "authoring sessions not available")
	}
	sess, err := s.authoringSessions.Get(ctx, sessionID)
	if err != nil {
		return authErr(500, "lookup session: %v", err)
	}
	if sess == nil {
		return authErr(404, "session not found")
	}
	if sess.ClosedAt != nil {
		return nil
	}
	if err := s.authoringSessions.Close(ctx, sess.ID, false); err != nil {
		return authErr(500, "close session: %v", err)
	}
	return nil
}

// startAuthoringPurgeLoop sweeps idle open authoring sessions on the configured
// ai.create_session_ttl cadence so a stale never-saved session stops blocking
// its source (the single-session-per-source rule would otherwise wedge it
// forever). A non-positive TTL disables the sweep. The loop exits when ctx is
// cancelled at daemon shutdown.
func (s *Server) startAuthoringPurgeLoop(ctx context.Context) {
	if s.authoringSessions == nil {
		return
	}
	s.cfgMu.RLock()
	ttl := time.Duration(0)
	if s.cfg != nil {
		ttl = s.cfg.AI.CreateSessionTTL
	}
	s.cfgMu.RUnlock()
	if ttl <= 0 {
		return
	}

	// Sweep at a fraction of the TTL so a session is cancelled within a
	// bounded window past its deadline rather than up to a full TTL late.
	interval := ttl / 4
	if interval < time.Minute {
		interval = time.Minute
	}

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := s.authoringSessions.PurgeExpired(ctx, ttl)
				if err != nil {
					s.log.Warn("authoring session purge failed", zap.Error(err))
					continue
				}
				if n > 0 {
					s.log.Info("purged idle authoring sessions", zap.Int("count", n))
				}
			}
		}
	}()
}

// WebUIBaseURL returns the scheme://host:port the browser would use to reach
// this daemon's web UI. TLS config flips the scheme; the host defaults to
// localhost since the daemon binds all interfaces and the CLI runs on the
// same machine.
func (s *Server) WebUIBaseURL() string {
	scheme := "http"
	s.cfgMu.RLock()
	if s.cfg != nil && s.cfg.Server.TLSCertFile != "" && s.cfg.Server.TLSKeyFile != "" {
		scheme = "https"
	}
	s.cfgMu.RUnlock()
	return fmt.Sprintf("%s://localhost:%d", scheme, s.port)
}

// statusFromAuthoringError extracts the HTTP status from an authoringError,
// defaulting to 500 for any other error type.
func statusFromAuthoringError(err error) int {
	var ae *authoringError
	if errors.As(err, &ae) {
		return ae.status
	}
	return 500
}

// sanitizeTaskName lowercases, replaces spaces with hyphens, and strips
// characters that are not alphanumeric or hyphen.
func sanitizeTaskName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	var b strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	return strings.Trim(b.String(), "-")
}

// resolveSourceKind returns "git", "local", or "taskset" for the named source.
func (s *Server) resolveSourceKind(name string) string {
	if s.sourceMgr == nil {
		return ""
	}
	for _, info := range s.sourceMgr.List() {
		if info.Name == name {
			return info.Type
		}
	}
	return ""
}
