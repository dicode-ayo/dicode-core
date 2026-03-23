// Package webui serves the REST API and HTMX-based dashboard.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server is the HTTP server for the web UI and REST API.
type Server struct {
	registry *registry.Registry
	engine   *trigger.Engine
	cfg      *config.Config
	log      *zap.Logger
	port     int
	srv      *http.Server
	baseTmpl *template.Template // parsed base.html only; cloned per render
}

// New creates a Server.
func New(port int, r *registry.Registry, eng *trigger.Engine, cfg *config.Config, log *zap.Logger) (*Server, error) {
	funcMap := template.FuncMap{
		"triggerLabel": triggerLabel,
		"fmtTime":      fmtTime,
		"fmtDuration":  fmtDuration,
		"slice":        func(s string, i, j int) string { return s[i:j] },
		"string":       fmt.Sprint,
	}
	base, err := template.New("base.html").Funcs(funcMap).ParseFS(templateFS, "templates/base.html")
	if err != nil {
		return nil, fmt.Errorf("parse base template: %w", err)
	}
	return &Server{
		registry: r,
		engine:   eng,
		cfg:      cfg,
		log:      log,
		port:     port,
		baseTmpl: base,
	}, nil
}

// Handler returns the HTTP handler (useful for testing without starting a server).
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)

	// UI routes
	r.Get("/", s.handleTaskList)
	r.Get("/tasks/{id}", s.handleTaskDetail)
	r.Get("/runs/{runID}", s.handleRunDetail)
	r.Get("/config", s.handleConfig)

	// REST API
	r.Get("/api/config", s.apiGetConfig)
	r.Get("/api/tasks", s.apiListTasks)
	r.Get("/api/tasks/{id}", s.apiGetTask)
	r.Post("/api/tasks/{id}/run", s.apiRunTask)
	r.Get("/api/tasks/{id}/runs", s.apiListRuns)
	r.Get("/api/runs/{runID}", s.apiGetRun)
	r.Get("/api/runs/{runID}/logs", s.apiGetLogs)

	// Webhook passthrough
	r.Post("/hooks/*", func(w http.ResponseWriter, req *http.Request) {
		req.URL.Path = "/hooks/" + chi.URLParam(req, "*")
		s.engine.WebhookHandler().ServeHTTP(w, req)
	})

	return r
}

// Start listens on the configured port until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.srv = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: s.Handler(),
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
	s.log.Info("webui listening", zap.Int("port", s.port))
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// --- UI handlers ---

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	s.render(w, "tasks.html", map[string]any{
		"Title": "Tasks",
		"Tasks": s.registry.All(),
	})
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spec, ok := s.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	runs, _ := s.registry.ListRuns(r.Context(), id, 20)
	s.render(w, "task.html", map[string]any{
		"Title": spec.Name,
		"Spec":  spec,
		"Runs":  runs,
	})
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	logs, _ := s.registry.GetRunLogs(r.Context(), runID)
	s.render(w, "run.html", map[string]any{
		"Title": "Run " + runID[:8],
		"Run":   run,
		"Logs":  logs,
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.render(w, "config.html", map[string]any{
		"Title": "Config",
		"Cfg":   s.cfg,
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, err := s.baseTmpl.Clone()
	if err != nil {
		http.Error(w, "template clone error", http.StatusInternalServerError)
		return
	}
	if _, err = t.ParseFS(templateFS, "templates/"+name); err != nil {
		s.log.Error("template parse", zap.String("template", name), zap.Error(err))
		http.Error(w, "template parse error", http.StatusInternalServerError)
		return
	}
	if err = t.ExecuteTemplate(w, "base.html", data); err != nil {
		s.log.Error("template render", zap.String("template", name), zap.Error(err))
	}
}

// --- REST API handlers ---

func (s *Server) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.cfg)
}

func (s *Server) apiListTasks(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.registry.All())
}

func (s *Server) apiGetTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}
	jsonOK(w, spec)
}

func (s *Server) apiRunTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	runID, err := s.engine.FireManual(r.Context(), id, nil)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]string{"runId": runID})
}

func (s *Server) apiListRuns(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	runs, err := s.registry.ListRuns(r.Context(), id, 50)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, runs)
}

func (s *Server) apiGetRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		jsonErr(w, "run not found", http.StatusNotFound)
		return
	}
	jsonOK(w, run)
}

func (s *Server) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	logs, err := s.registry.GetRunLogs(r.Context(), runID)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// For HTMX polling return plain text; for API callers return JSON.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/plain")
		for _, l := range logs {
			fmt.Fprintf(w, "[%s] %s %s\n", l.Level, fmtTime(l.Ts), l.Message)
		}
		return
	}
	jsonOK(w, logs)
}

// --- helpers ---

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func triggerLabel(tc any) string {
	// tc is task.TriggerConfig — use fmt to inspect its fields via reflection would
	// require importing task. Instead the template passes the whole Trigger field
	// and we return a human-readable string by type-switching the map form.
	type triggerable interface {
		GetCron() string
	}
	switch v := tc.(type) {
	case map[string]any:
		if c, _ := v["cron"].(string); c != "" {
			return "cron: " + c
		}
		if p, _ := v["webhook"].(string); p != "" {
			return "webhook"
		}
		if m, _ := v["manual"].(bool); m {
			return "manual"
		}
	default:
		_ = v
	}
	return "—"
}

func fmtTime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}

func fmtDuration(start time.Time, end *time.Time) string {
	if end == nil {
		return "running…"
	}
	d := end.Sub(start).Round(time.Millisecond)
	return d.String()
}
