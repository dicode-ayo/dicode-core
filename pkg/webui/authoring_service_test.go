package webui

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
)

// TestIsUniqueConstraint_RealDriverError pins isUniqueConstraint against the
// actual error the SQLite driver returns when the single-open-session-per-source
// partial unique index rejects a second insert. If the driver's wording drifts,
// the EditTask race-loser path would silently fall back to a 500 — this guards it.
func TestIsUniqueConstraint_RealDriverError(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	now := time.Now()
	first := AuthoringSession{
		ID: "s1", Kind: "edit", Source: "ai-scratch", TaskID: "ai-scratch/a",
		CreatedAt: now, LastTurnAt: now,
	}
	if err := store.Create(ctx, first); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Second open session for the same source must violate the partial index.
	second := AuthoringSession{
		ID: "s2", Kind: "edit", Source: "ai-scratch", TaskID: "ai-scratch/b",
		CreatedAt: now, LastTurnAt: now,
	}
	err := store.Create(ctx, second)
	if err == nil {
		t.Fatal("second Create succeeded, want UNIQUE constraint violation")
	}
	if !isUniqueConstraint(err) {
		t.Fatalf("isUniqueConstraint(%v) = false; driver wording drifted", err)
	}
}

// TestEditTask_ConcurrentSameTaskNoServerError fires many concurrent edits for
// the same task+source and asserts none get a 500. The single-session index
// prevents double-creation; the race loser must re-resolve to the open session
// (202), and at most one different-task collision would surface 409 — never 500.
func TestEditTask_ConcurrentSameTaskNoServerError(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	sessions := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := srv.EditTask(context.Background(), "", "ai-scratch/hello")
			errs[i] = err
			sessions[i] = res.SessionID
		}(i)
	}
	wg.Wait()

	var sessID string
	for i, err := range errs {
		if err != nil {
			if status := statusFromAuthoringError(err); status >= 500 {
				t.Fatalf("goroutine %d got server error %d: %v", i, status, err)
			}
			t.Fatalf("goroutine %d got unexpected error: %v", i, err)
		}
		if sessID == "" {
			sessID = sessions[i]
		} else if sessions[i] != sessID {
			t.Fatalf("goroutine %d resolved to session %q, want shared %q", i, sessions[i], sessID)
		}
	}
}

// TestWebUIBaseURL covers the precedence every outbound link depends on:
// server.public_url wins outright, TLS only picks the scheme for the loopback
// fallback. A notification that leaves the machine carries whatever this
// returns, so a wrong answer here is a link the recipient cannot follow.
func TestWebUIBaseURL(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.ServerConfig
		want string
	}{
		{
			name: "loopback fallback",
			cfg:  config.ServerConfig{Port: 8080},
			want: "http://localhost:8080",
		},
		{
			name: "tls flips the fallback scheme",
			cfg:  config.ServerConfig{Port: 8080, TLSCertFile: "cert.pem", TLSKeyFile: "key.pem"},
			want: "https://localhost:8080",
		},
		{
			name: "public_url wins",
			cfg:  config.ServerConfig{Port: 8080, Auth: true, PublicURL: "https://dicode.example.com"},
			want: "https://dicode.example.com",
		},
		{
			name: "public_url keeps its own scheme and port over the TLS guess",
			cfg: config.ServerConfig{
				Port: 8080, TrustProxy: true,
				TLSCertFile: "cert.pem", TLSKeyFile: "key.pem",
				PublicURL: "http://dicode.lan:9000",
			},
			want: "http://dicode.lan:9000",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServerWithSourceMgr(t, &config.Config{Server: tc.cfg}, "", nil)
			if got := srv.WebUIBaseURL(); got != tc.want {
				t.Errorf("WebUIBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
