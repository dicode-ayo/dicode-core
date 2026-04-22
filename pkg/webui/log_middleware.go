package webui

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
)

// zapLogFormatter routes chi's per-request log entries through a zap.Logger.
// Level by status: 2xx/3xx → Debug, 4xx → Warn, 5xx → Error. Panics from
// middleware.Recoverer are logged at Error with the stack.
type zapLogFormatter struct{ log *zap.Logger }

func (f *zapLogFormatter) NewLogEntry(r *http.Request) middleware.LogEntry {
	return &zapLogEntry{
		log:    f.log,
		method: r.Method,
		path:   r.URL.Path,
		remote: r.RemoteAddr,
		reqID:  middleware.GetReqID(r.Context()),
	}
}

type zapLogEntry struct {
	log                         *zap.Logger
	method, path, remote, reqID string
}

func (e *zapLogEntry) Write(status, bytes int, _ http.Header, elapsed time.Duration, _ interface{}) {
	fields := []zap.Field{
		zap.String("method", e.method),
		zap.String("path", e.path),
		zap.Int("status", status),
		zap.Int("bytes", bytes),
		zap.Duration("dur", elapsed),
		zap.String("remote", e.remote),
		zap.String("req_id", e.reqID),
	}
	switch {
	case status >= 500:
		e.log.Error("http", fields...)
	case status >= 400:
		e.log.Warn("http", fields...)
	default:
		e.log.Debug("http", fields...)
	}
}

func (e *zapLogEntry) Panic(v interface{}, stack []byte) {
	e.log.Error("http panic",
		zap.String("method", e.method),
		zap.String("path", e.path),
		zap.String("req_id", e.reqID),
		zap.Any("panic", v),
		zap.ByteString("stack", stack),
	)
}
