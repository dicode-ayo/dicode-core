package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/config"
	gitSource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// sourceNameRe restricts a source's `spec.entries` key to a safe identifier.
// The name is used verbatim as a YAML map key and as the first path segment
// of every task ID under that source (namespace/task-dir — see
// pkg/taskset/resolver.go joinNamespace), so it must not contain '/' or other
// characters that could be misread as a path separator or break YAML/JSON
// round-tripping. Mirrors the identifier pattern already used for other
// user-supplied tokens embedded into a namespace elsewhere in this codebase
// (e.g. container volume names in pkg/runtime/containersec/policy.go).
var sourceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// validateSourceName returns an error describing why name is not a valid
// source (spec.entries) name, or nil if it is valid.
func validateSourceName(name string) error {
	if !sourceNameRe.MatchString(name) {
		return fmt.Errorf("name must start with a letter or digit and contain only letters, digits, '-', '_', or '.' (max 64 characters)")
	}
	return nil
}

// SourceInfo is the JSON representation of a source for the API and for the
// MCP task that consumes /api/sources. LastPullAt is a pointer because
// `time.Time` + `omitempty` does NOT omit the zero value — it serializes as
// "0001-01-01T00:00:00Z", which is truthy in JS and causes the frontend to
// render a spurious status dot for every local / never-pulled source.
type SourceInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	URL     string `json:"url,omitempty"`
	Path    string `json:"path,omitempty"`
	Branch  string `json:"branch,omitempty"`
	DevMode bool   `json:"dev_mode"`
	DevPath string `json:"dev_path,omitempty"`

	LastPullAt    *time.Time `json:"last_pull_at,omitempty"`
	LastPullOK    bool       `json:"last_pull_ok,omitempty"`
	LastPullError string     `json:"last_pull_error,omitempty"`

	// FailedCount is the number of entries under this source that currently
	// fail to resolve/load/validate (#649) — a task.yaml syntax error, for
	// example. Failures is the same set with per-entry detail; both are
	// omitted (rather than sent empty) when there are none, so the frontend's
	// `if (src.failed_count)` guards read cleanly. A source can have
	// FailedCount > 0 and LastPullOK true at the same time — a bad pull and a
	// bad task.yaml are independent failure modes, and both must suppress the
	// "all clear" (green) status dot.
	FailedCount int                `json:"failed_count,omitempty"`
	Failures    []task.LoadFailure `json:"failures,omitempty"`
}

// SourceManager tracks taskset sources by name and provides dev mode control.
// It is the single point of truth for source state visible to the REST API.
//
// cfgMu protects reads of cfg fields (Spec.Entries) — it is shared with the
// Server so writers there serialise correctly with readers here. Pass nil
// only in tests that don't exercise concurrent cfg mutations.
type SourceManager struct {
	mu       sync.RWMutex
	cfgMu    *sync.RWMutex
	cfg      *config.Config
	tasksets map[string]*taskset.Source // source name → live taskset.Source
	dataDir  string
	log      *zap.Logger
}

// NewSourceManager creates a SourceManager.
// tasksetSources maps source name to the live *taskset.Source (may be nil map for non-taskset setups).
// The cfg mutex is bound separately via BindCfgMutex once the Server is built;
// chicken-and-egg between daemon initialisation order is why we don't take it here.
func NewSourceManager(cfg *config.Config, tasksetSources map[string]*taskset.Source, dataDir string, log *zap.Logger) *SourceManager {
	if tasksetSources == nil {
		tasksetSources = make(map[string]*taskset.Source)
	}
	return &SourceManager{
		cfg:      cfg,
		tasksets: tasksetSources,
		dataDir:  dataDir,
		log:      log,
	}
}

// BindCfgMutex wires the Server's cfg mutex into this SourceManager so reads
// of m.cfg.Spec.Entries serialise with concurrent writers. Call once, after
// the Server has been constructed and before the HTTP handler is exposed.
func (m *SourceManager) BindCfgMutex(mu *sync.RWMutex) {
	m.cfgMu = mu
}

// SetCfg replaces the config pointer. Must be called under cfgMu.Lock()
// (the same mutex wired by BindCfgMutex).
func (m *SourceManager) SetCfg(cfg *config.Config) {
	m.cfg = cfg
}

// rLockCfg / rUnlockCfg let SourceManager methods uniformly acquire the
// shared cfg lock; they're no-ops if cfgMu is nil (test setups).
func (m *SourceManager) rLockCfg() {
	if m.cfgMu != nil {
		m.cfgMu.RLock()
	}
}
func (m *SourceManager) rUnlockCfg() {
	if m.cfgMu != nil {
		m.cfgMu.RUnlock()
	}
}

// Register adds or replaces a named taskset.Source at runtime (e.g. after apiAddSource).
func (m *SourceManager) Register(name string, src *taskset.Source) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasksets[name] = src
}

// Get returns the live taskset.Source for a source name, or (nil, false) if
// the name is unknown. Used by apiPatchTaskOverrides to signal a refresh
// after writing a new override to dicode.yaml.
func (m *SourceManager) Get(name string) (*taskset.Source, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src, ok := m.tasksets[name]
	return src, ok
}

// List returns info for all configured sources including their live dev mode state.
func (m *SourceManager) List() []SourceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	m.rLockCfg()
	defer m.rUnlockCfg()
	entries := m.cfg.Spec.Entries
	out := make([]SourceInfo, 0, len(entries))
	for name, entry := range entries {
		if entry == nil || entry.Ref == nil {
			continue
		}
		ref := entry.Ref
		info := SourceInfo{
			Name:   name,
			URL:    ref.URL,
			Path:   ref.Path,
			Branch: ref.Branch,
		}
		if src, ok := m.tasksets[name]; ok {
			info.Type = "taskset"
			info.DevMode = src.DevMode()
			info.DevPath = src.DevRootPath()
			ps := src.PullStatus()
			// Only populate the pull-health fields when a pull has
			// actually been attempted — leaving them nil lets the
			// frontend's `if (!src.last_pull_at)` guard suppress the dot.
			if !ps.LastPullAt.IsZero() {
				t := ps.LastPullAt
				info.LastPullAt = &t
				info.LastPullOK = ps.OK
				info.LastPullError = ps.Error
			}
			if fails := src.LoadFailures(); len(fails) > 0 {
				info.FailedCount = len(fails)
				info.Failures = make([]task.LoadFailure, 0, len(fails))
				for _, f := range fails {
					info.Failures = append(info.Failures, f)
				}
				sort.Slice(info.Failures, func(i, j int) bool { return info.Failures[i].ID < info.Failures[j].ID })
			}
		} else if ref.IsGit() {
			info.Type = "git"
		} else {
			info.Type = "local"
		}
		out = append(out, info)
	}
	return out
}

// LoadFailures aggregates the per-source load-failure state (#649) across
// every live taskset source into one ID → LoadFailure map. Used by
// apiListTasks to merge failing entries into GET /api/tasks alongside the
// registry's real registered tasks.
func (m *SourceManager) LoadFailures() map[string]task.LoadFailure {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]task.LoadFailure)
	for _, src := range m.tasksets {
		for id, f := range src.LoadFailures() {
			out[id] = f
		}
	}
	return out
}

// SetDevMode enables or disables dev mode for the named taskset source.
func (m *SourceManager) SetDevMode(ctx context.Context, name string, enabled bool, opts taskset.DevModeOpts) error {
	m.mu.RLock()
	src, ok := m.tasksets[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("source %q not found or not a taskset source", name)
	}
	m.log.Info("dev mode toggled",
		zap.String("source", name),
		zap.Bool("enabled", enabled),
		zap.String("local_path", opts.LocalPath),
		zap.String("branch", opts.Branch),
	)
	return src.SetDevMode(ctx, enabled, opts)
}

// ResolveRepoPath returns the on-disk repo path for the named taskset source.
// Implements ipc.RepoPathResolver.
func (m *SourceManager) ResolveRepoPath(name string) (string, error) {
	m.mu.RLock()
	src, ok := m.tasksets[name]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("source %q not found or not a taskset source", name)
	}
	p := src.RepoPath()
	if p == "" {
		return "", fmt.Errorf("source %q repo path not yet resolved (Start not called?)", name)
	}
	return p, nil
}

// ListBranches returns remote branches for the named git source.
func (m *SourceManager) ListBranches(ctx context.Context, name string) ([]string, error) {
	m.rLockCfg()
	entry := m.cfg.Spec.Entries[name]
	if entry == nil || entry.Ref == nil || !entry.Ref.IsGit() {
		m.rUnlockCfg()
		return nil, fmt.Errorf("source %q not found or not a git source", name)
	}
	url := entry.Ref.URL
	tokenEnv := entry.Ref.Auth.TokenEnv
	m.rUnlockCfg()
	return gitSource.ListBranches(ctx, url, tokenEnv)
}

// --- HTTP handlers ---

func (s *Server) apiListSources(w http.ResponseWriter, r *http.Request) {
	if s.sourceMgr == nil {
		jsonOK(w, []SourceInfo{})
		return
	}
	jsonOK(w, s.sourceMgr.List())
}

func (s *Server) apiSetDevMode(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var body struct {
		Enabled   bool   `json:"enabled"`
		LocalPath string `json:"local_path"`
		Branch    string `json:"branch"`
		Base      string `json:"base"`
		RunID     string `json:"run_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.sourceMgr == nil {
		jsonErr(w, "source manager not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.sourceMgr.SetDevMode(r.Context(), name, body.Enabled, taskset.DevModeOpts{
		LocalPath: body.LocalPath,
		Branch:    body.Branch,
		Base:      body.Base,
		RunID:     body.RunID,
	}); err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{
		"source":     name,
		"dev_mode":   body.Enabled,
		"local_path": body.LocalPath,
		"branch":     body.Branch,
		"base":       body.Base,
		"run_id":     body.RunID,
	})
}

type commitPushRequest struct {
	Message      string   `json:"message"`
	Branch       string   `json:"branch"`
	BranchPrefix string   `json:"branch_prefix"`
	AllowMain    bool     `json:"allow_main"`
	Files        []string `json:"files"`
	AuthorName   string   `json:"author_name"`
	AuthorEmail  string   `json:"author_email"`
	AuthTokenEnv string   `json:"auth_token_env"`
}

func (s *Server) apiCommitPush(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var req commitPushRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	// auth_token_env is not supported via REST — the REST endpoint has no
	// task-scoped permissions.env to validate against, so any env var name
	// could exfiltrate daemon secrets. Callers that need auth must use the
	// dicode.git.commit_push IPC method from a task with the env var declared
	// in permissions.env.
	if req.AuthTokenEnv != "" {
		jsonErr(w, "auth_token_env is only supported via IPC; use the dicode.git.commit_push SDK from a task", http.StatusBadRequest)
		return
	}
	if s.sourceMgr == nil {
		jsonErr(w, "source manager not available", http.StatusServiceUnavailable)
		return
	}
	repoPath, err := s.sourceMgr.ResolveRepoPath(name)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusNotFound)
		return
	}
	hash, err := gitSource.CommitPush(r.Context(), repoPath, gitSource.CommitPushOptions{
		Message:      req.Message,
		Branch:       req.Branch,
		BranchPrefix: req.BranchPrefix,
		AllowMain:    req.AllowMain,
		Files:        req.Files,
		Author: gitSource.Signature{
			Name:  req.AuthorName,
			Email: req.AuthorEmail,
		},
		AuthToken: "", // auth_token_env blocked above for REST callers
	})
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"commit": hash})
}

func (s *Server) apiListSourceBranches(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if s.sourceMgr == nil {
		jsonErr(w, "source manager not configured", http.StatusServiceUnavailable)
		return
	}
	branches, err := s.sourceMgr.ListBranches(r.Context(), name)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, branches)
}
