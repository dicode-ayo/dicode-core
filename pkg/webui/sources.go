package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/mcp"
	gitSource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// SourceInfo is the JSON representation of a source for the API.
// It is an alias of mcp.SourceEntry so SourceManager directly satisfies mcp.SourceLister.
type SourceInfo = mcp.SourceEntry

// SourceManager tracks taskset sources by name and provides dev mode control.
// It is the single point of truth for source state visible to the REST API and MCP.
type SourceManager struct {
	mu       sync.RWMutex
	cfg      *config.Config
	tasksets map[string]*taskset.Source // source name → live taskset.Source
	dataDir  string
	log      *zap.Logger
}

// NewSourceManager creates a SourceManager.
// tasksetSources maps source name to the live *taskset.Source (may be nil map for non-taskset setups).
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

// Register adds or replaces a named taskset.Source at runtime (e.g. after apiAddSource).
func (m *SourceManager) Register(name string, src *taskset.Source) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasksets[name] = src
}

// List returns info for all configured sources including their live dev mode state.
// Satisfies mcp.SourceLister.
func (m *SourceManager) List() []mcp.SourceEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]mcp.SourceEntry, 0, len(m.cfg.Sources))
	for _, sc := range m.cfg.Sources {
		name := sourceName(sc)
		info := mcp.SourceEntry{
			Name:   name,
			URL:    sc.URL,
			Path:   sc.Path,
			Branch: sc.Branch,
		}
		if src, ok := m.tasksets[name]; ok {
			info.Type = "taskset"
			info.DevMode = src.DevMode()
			info.DevPath = src.DevRootPath()
		} else if sc.URL != "" {
			info.Type = "git"
		} else {
			info.Type = "local"
		}
		out = append(out, info)
	}
	return out
}

// SetDevMode enables or disables dev mode for the named taskset source.
// localPath, if non-empty, overrides the root entry point for the duration of dev mode.
func (m *SourceManager) SetDevMode(ctx context.Context, name string, enabled bool, localPath string) error {
	m.mu.RLock()
	src, ok := m.tasksets[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("source %q not found or not a taskset source", name)
	}
	m.log.Info("dev mode toggled",
		zap.String("source", name),
		zap.Bool("enabled", enabled),
		zap.String("local_path", localPath),
	)
	return src.SetDevMode(ctx, enabled, localPath)
}

// ListBranches returns remote branches for the named git source.
func (m *SourceManager) ListBranches(ctx context.Context, name string) ([]string, error) {
	for _, sc := range m.cfg.Sources {
		if sourceName(sc) == name && sc.URL != "" {
			return gitSource.ListBranches(ctx, sc.URL, sc.Auth.TokenEnv)
		}
	}
	return nil, fmt.Errorf("source %q not found or not a git source", name)
}

// sourceName derives the canonical name for a SourceConfig (same logic as buildTaskSetSource).
func sourceName(sc config.SourceConfig) string {
	if sc.Name != "" {
		return sc.Name
	}
	base := sc.URL
	if base == "" {
		base = sc.Path
	}
	base = strings.TrimRight(base, "/")
	name := filepath.Base(base)
	if ext := filepath.Ext(name); ext == ".yaml" || ext == ".yml" {
		name = strings.TrimSuffix(name, ext)
	}
	return name
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.sourceMgr == nil {
		jsonErr(w, "source manager not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.sourceMgr.SetDevMode(r.Context(), name, body.Enabled, body.LocalPath); err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{
		"source":     name,
		"dev_mode":   body.Enabled,
		"local_path": body.LocalPath,
	})
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
