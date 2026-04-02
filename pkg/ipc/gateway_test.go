package ipc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// ── Gateway unit tests ────────────────────────────────────────────────────────

func TestGateway_Register_And_Route(t *testing.T) {
	g := NewGateway()
	g.Register("/hooks/push", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("push"))
	}))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/push", nil))
	if w.Code != 200 || w.Body.String() != "push" {
		t.Errorf("unexpected response: %d %s", w.Code, w.Body.String())
	}
}

func TestGateway_PrefixMatch(t *testing.T) {
	g := NewGateway()
	g.Register("/hooks/ui", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(r.URL.Path))
	}))

	for _, path := range []string{"/hooks/ui", "/hooks/ui/", "/hooks/ui/style.css"} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Errorf("path %s: expected 200, got %d", path, w.Code)
		}
	}
}

func TestGateway_LongestPrefixWins(t *testing.T) {
	g := NewGateway()
	g.Register("/hooks", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("short"))
	}))
	g.Register("/hooks/push", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("long"))
	}))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/push", nil))
	if w.Body.String() != "long" {
		t.Errorf("expected longer prefix to win; got %q", w.Body.String())
	}
}

func TestGateway_NoMatch_Returns404(t *testing.T) {
	g := NewGateway()
	g.Register("/hooks/push", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/hooks/other", nil))
	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestGateway_Unregister(t *testing.T) {
	g := NewGateway()
	g.Register("/hooks/push", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	g.Unregister("/hooks/push")

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/push", nil))
	if w.Code != 404 {
		t.Errorf("expected 404 after unregister, got %d", w.Code)
	}
}

func TestGateway_ReplaceHandler(t *testing.T) {
	g := NewGateway()
	g.Register("/hooks/push", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v1"))
	}))
	g.Register("/hooks/push", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v2"))
	}))

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/push", nil))
	if w.Body.String() != "v2" {
		t.Errorf("expected replaced handler; got %q", w.Body.String())
	}
}

// ── IPC http.register integration test ───────────────────────────────────────

func TestServer_HTTPRegister_And_Respond(t *testing.T) {
	g := NewGateway()
	e := newTestEnv(t)

	// Start an IPC server for a daemon task with the gateway attached.
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	spec := &task.Spec{ID: "my-daemon", Trigger: task.TriggerConfig{Daemon: true}}
	srv := New(runID, "my-daemon", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil, "", "", "")
	srv.SetGateway(g)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)
	if !hasCap(caps, CapHTTPRegister) {
		t.Fatalf("expected http.register cap for daemon task; got %v", caps)
	}

	// Task registers /hooks/my-daemon in the gateway.
	sendMsg(t, conn, map[string]any{"id": "1", "method": "http.register", "pattern": "/hooks/my-daemon"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("http.register error: %v", resp["error"])
	}

	// Simulate an inbound HTTP request through the gateway in a goroutine.
	// The task reads the pushed HTTPInboundRequest, then sends http.respond.
	type httpResult struct {
		code int
		body string
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		req := httptest.NewRequest("GET", "/hooks/my-daemon", nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		resultCh <- httpResult{w.Code, w.Body.String()}
	}()

	// The task reads the inbound push from the server.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var inbound map[string]any
	if err := readMsg(conn, &inbound); err != nil {
		t.Fatalf("expected HTTPInboundRequest push: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	rid, _ := inbound["requestID"].(string)
	if rid == "" {
		t.Fatalf("missing requestID in push: %v", inbound)
	}

	// Task responds.
	sendMsg(t, conn, map[string]any{
		"method":    "http.respond",
		"requestID": rid,
		"status":    200,
		"respBody":  []byte("hello from task"),
	})

	select {
	case res := <-resultCh:
		if res.code != 200 {
			t.Errorf("expected 200, got %d", res.code)
		}
		if !strings.Contains(res.body, "hello from task") {
			t.Errorf("unexpected body: %q", res.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for gateway response")
	}
}

func TestServer_HTTPRegister_RequiresDaemonSpec(t *testing.T) {
	g := NewGateway()
	e := newTestEnv(t)

	// Non-daemon task — should NOT get CapHTTPRegister.
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "regular-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil, "", "", "")
	srv.SetGateway(g)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)
	if hasCap(caps, CapHTTPRegister) {
		t.Error("non-daemon task should not receive http.register cap")
	}

	// Even if the client sends http.register, it gets permission denied.
	sendMsg(t, conn, map[string]any{"id": "1", "method": "http.register", "pattern": "/hooks/x"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Error("expected permission denied for http.register without cap")
	}
}
