package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// newObservedLogger returns a zap.Logger that records every entry at
// DebugLevel-or-higher into an observer.ObservedLogs for assertion.
func newObservedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// runRequest wraps an http.HandlerFunc in chi + RequestID + our middleware,
// fires one request, and returns the captured log entries.
func runRequest(t *testing.T, log *zap.Logger, handler http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RequestLogger(&zapLogFormatter{log: log}))
	r.Handle("/*", handler)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRequestLogger_2xx_Debug(t *testing.T) {
	log, logs := newObservedLogger()
	runRequest(t, log, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, httptest.NewRequest(http.MethodGet, "/ok", nil))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log entry, got %d", len(entries))
	}
	if entries[0].Level != zapcore.DebugLevel {
		t.Errorf("level = %s; want Debug", entries[0].Level)
	}
	fields := entries[0].ContextMap()
	if got, ok := fields["status"].(int64); !ok || got != 200 {
		t.Errorf("status field = %v; want 200", fields["status"])
	}
	if fields["method"] != "GET" {
		t.Errorf("method field = %v; want GET", fields["method"])
	}
	if fields["path"] != "/ok" {
		t.Errorf("path field = %v; want /ok", fields["path"])
	}
}

func TestRequestLogger_4xx_Warn(t *testing.T) {
	log, logs := newObservedLogger()
	runRequest(t, log, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}, httptest.NewRequest(http.MethodGet, "/missing", nil))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log entry, got %d", len(entries))
	}
	if entries[0].Level != zapcore.WarnLevel {
		t.Errorf("level = %s; want Warn", entries[0].Level)
	}
	if got, ok := entries[0].ContextMap()["status"].(int64); !ok || got != 404 {
		t.Errorf("status = %v; want 404", entries[0].ContextMap()["status"])
	}
}

func TestRequestLogger_5xx_Error(t *testing.T) {
	log, logs := newObservedLogger()
	runRequest(t, log, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, httptest.NewRequest(http.MethodGet, "/boom", nil))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log entry, got %d", len(entries))
	}
	if entries[0].Level != zapcore.ErrorLevel {
		t.Errorf("level = %s; want Error", entries[0].Level)
	}
	if got, ok := entries[0].ContextMap()["status"].(int64); !ok || got != 500 {
		t.Errorf("status = %v; want 500", entries[0].ContextMap()["status"])
	}
}

func TestRequestLogger_ReqID_Propagates(t *testing.T) {
	log, logs := newObservedLogger()
	rec := runRequest(t, log, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, httptest.NewRequest(http.MethodGet, "/id", nil))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log entry, got %d", len(entries))
	}
	got, ok := entries[0].ContextMap()["req_id"].(string)
	if !ok || got == "" {
		t.Fatalf("req_id field missing or empty: %v", entries[0].ContextMap()["req_id"])
	}
	if header := rec.Header().Get("X-Request-Id"); header != "" && header != got {
		t.Errorf("req_id field %q does not match X-Request-Id header %q", got, header)
	}
}

func TestRequestLogger_Panic_Logged(t *testing.T) {
	log, logs := newObservedLogger()

	// RequestLogger must be OUTER to Recoverer: chi's Recoverer reads the
	// LogEntry from the request context, which RequestLogger populates.
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RequestLogger(&zapLogFormatter{log: log}))
	r.Use(middleware.Recoverer)
	r.Handle("/*", http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))

	var panicEntry *observer.LoggedEntry
	for i := range logs.All() {
		e := logs.All()[i]
		if e.Message == "http panic" {
			panicEntry = &e
			break
		}
	}
	if panicEntry == nil {
		t.Fatalf("no 'http panic' entry in logs: %+v", logs.All())
	}
	if panicEntry.Level != zapcore.ErrorLevel {
		t.Errorf("level = %s; want Error", panicEntry.Level)
	}
	if panicEntry.ContextMap()["panic"] == nil {
		t.Error("missing 'panic' field")
	}
}
