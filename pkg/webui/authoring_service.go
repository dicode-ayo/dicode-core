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
	// A git source resolves from the pull cache, and gitops pulls with
	// Force: true, so a scaffold there survives only until the next upstream
	// commit — which discards the task files and the taskset entry alike,
	// leaving an unregistered directory behind. Dev mode moves
	// RootTaskSetPath into a clone, which is a working tree and writable.
	if src.IsGit() && !src.DevMode() {
		return ipc.AuthoringCreateResult{}, authErr(409,
			"source %q is git-backed: files scaffolded into its pull cache are discarded by the next pull — enable dev mode on the source, or create into a local source", source)
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

	tasksetPath := src.RootTaskSetPath()
	if tasksetPath == "" {
		return ipc.AuthoringCreateResult{}, authErr(503, "source has no repo path; is it started?")
	}
	taskDir := scaffoldDir(tasksetPath, name)
	// Exclusive create, not Stat-then-MkdirAll: two concurrent CreateTask
	// calls for the same (source, name) — a double-submitted request, a
	// client retry racing the original — could otherwise both pass the Stat
	// check before either had written task.yaml, and both proceed to
	// scaffold/register the same name. os.Mkdir is atomic, so only one
	// caller can win taskDir; the loser gets EEXIST and a 409, matching the
	// existing conflict contract without a TOCTOU window.
	if err := os.Mkdir(taskDir, 0755); err != nil {
		if os.IsExist(err) {
			return ipc.AuthoringCreateResult{}, authErr(409, "task %q already exists in source %q", name, source)
		}
		return ipc.AuthoringCreateResult{}, authErr(500, "create task dir: %v", err)
	}
	// Exclusive Mkdir means this call owns taskDir outright — nothing else
	// could have raced it into existence — so any failure past this point
	// removes the whole directory rather than leaving a partial scaffold
	// that would permanently 409 every retry of this name (a write failing
	// after task.yaml landed but before task.js, or registration failing
	// after both files landed, previously left exactly that trap). Success
	// is the only path that must NOT clean up.
	success := false
	defer func() {
		if !success {
			os.RemoveAll(taskDir)
		}
	}()

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
		if errors.Is(err, taskset.ErrEntryConflict) {
			return ipc.AuthoringCreateResult{}, authErr(409, "task name %q is already bound to another path in source %q", name, source)
		}
		return ipc.AuthoringCreateResult{}, authErr(500, "register task in taskset: %v", err)
	}

	success = true
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
		return s.editResultFor(sess), nil
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
		ID:     sessID,
		Kind:   "edit",
		Source: source,
		TaskID: taskID,
		// Resolved once, at open, rather than per turn: this is the
		// boundary the session's writes are confined to, and a boundary
		// that moves when the registry re-resolves the task elsewhere is
		// not a boundary.
		SandboxPath: s.taskDirFor(taskID),
		CreatedAt:   now,
		LastTurnAt:  now,
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

	// AgentSessionID stays empty: this is a brand-new session, there is no
	// prior AI turn to have set it.
	return s.editResultFor(&sess), nil
}

// resumeOrConflict resumes an existing open session for a source when it is the
// same task, or returns the 409 single-session conflict when the open session
// belongs to a different task.
func (s *Server) resumeOrConflict(ctx context.Context, existing *AuthoringSession, source, taskID string) (ipc.AuthoringEditResult, error) {
	if existing.TaskID != "" && existing.TaskID != taskID {
		return ipc.AuthoringEditResult{}, authErr(409, "source %q already has an open session %s for task %q (#283)", source, existing.ID, existing.TaskID)
	}
	_ = s.authoringSessions.UpdateLastTurn(ctx, existing.ID)
	return s.editResultFor(existing), nil
}

// editResultFor projects a session onto the IPC wire shape every EditTask
// path returns.
func (s *Server) editResultFor(sess *AuthoringSession) ipc.AuthoringEditResult {
	return ipc.AuthoringEditResult{
		SessionID:      sess.ID,
		TaskID:         sess.TaskID,
		SandboxPath:    sess.SandboxPath,
		TaskDir:        s.taskDirFor(sess.TaskID),
		Source:         sess.Source,
		SourceKind:     s.resolveSourceKind(sess.Source),
		AgentSessionID: derefOrEmpty(sess.AgentSessionID),
	}
}

// taskDirFor resolves the absolute directory holding taskID's files, or ""
// when it can't be determined. The registry is authoritative, but a task
// scaffolded seconds ago isn't in it until the reconciler's next sync, so fall
// back to the layout CreateTask scaffolds into.
func (s *Server) taskDirFor(taskID string) string {
	if s.registry != nil {
		if spec, ok := s.registry.Get(taskID); ok && spec.TaskDir != "" {
			return spec.TaskDir
		}
	}
	if s.sourceMgr == nil {
		return ""
	}
	source, name, ok := strings.Cut(taskID, "/")
	if !ok || source == "" || name == "" {
		return ""
	}
	src, ok := s.sourceMgr.Get(source)
	if !ok {
		return ""
	}
	tasksetPath := src.RootTaskSetPath()
	if tasksetPath == "" {
		return ""
	}
	return scaffoldDir(tasksetPath, name)
}

// writeTaskFileTaskID is the authoring agents' only route to disk. Its
// declared roots bound every write a session makes, so they also decide
// whether a session has a usable sandbox at all.
const writeTaskFileTaskID = "buildin/write-task-file"

// SandboxWritable reports whether dir is a directory the authoring write tool
// will accept a write into, along with the roots it checked against so a
// refusal can name them. The roots are read off the registered spec rather
// than a constant, so an operator override that widens them widens this check
// with it.
func (s *Server) SandboxWritable(dir string) (bool, []string) {
	roots := s.writeToolRoots()
	if dir == "" {
		return false, roots
	}
	dir = filepath.Clean(dir)
	for _, root := range roots {
		// The tool writes <root>/<task>/<file> — a task's files live directly
		// in a directory of their own under a root — so a sandbox nested any
		// deeper, or a root itself, is one it refuses.
		if filepath.Dir(dir) == root {
			return true, roots
		}
	}
	return false, roots
}

// writeToolRoots returns the cleaned writable roots buildin/write-task-file
// resolved to, or nil when the task is not registered.
func (s *Server) writeToolRoots() []string {
	if s.registry == nil {
		return nil
	}
	spec, ok := s.registry.Get(writeTaskFileTaskID)
	if !ok {
		return nil
	}
	for _, e := range spec.Permissions.Env {
		if e.Name != "DICODE_TASK_FILE_ROOTS" {
			continue
		}
		var roots []string
		for _, r := range strings.Split(e.Value, ",") {
			if r = strings.TrimSpace(r); r != "" {
				roots = append(roots, filepath.Clean(r))
			}
		}
		return roots
	}
	return nil
}

// scaffoldDir returns the directory a task named name occupies in the source
// rooted at tasksetPath. The taskset entry points at "./<name>/task.yaml",
// which the resolver reads relative to the taskset file's own directory, so
// scaffolding anywhere else makes the task unresolvable.
func scaffoldDir(tasksetPath, name string) string {
	return filepath.Join(filepath.Dir(tasksetPath), name)
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

// UpdateAgentSessionID persists the underlying ai-agent run's own session
// id onto the named authoring session (#568). Implements
// ipc.AuthoringService for pkg/ipc's handleTaskEdit, which calls this after
// a successful AI turn so the next `dicode task edit` on the same session
// carries the same run-group correlation id — not conversational memory,
// see the handleTaskEdit doc comment in pkg/ipc/control.go. See
// authoringSessionStore.UpdateAgentSessionID for the blank-is-noop
// semantics.
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
