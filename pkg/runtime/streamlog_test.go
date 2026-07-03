package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"go.uber.org/zap"
)

func newStreamTestDeps(t *testing.T) (*BridgeDeps, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	return &BridgeDeps{Registry: reg, Log: zap.NewNop()}, reg
}

// TestStreamRunLog_AppendsRedactedLines verifies the contract both runtimes'
// scanner goroutines implemented: every line lands in the run log at the
// requested level, secrets are redacted, and wg.Done fires so callers can
// wait for the flush before returning.
func TestStreamRunLog_AppendsRedactedLines(t *testing.T) {
	deps, reg := newStreamTestDeps(t)
	red := secrets.NewRedactor(map[string]string{"TOKEN": "s3cret-value"})

	input := "hello\nleaking s3cret-value now\n"
	var wg sync.WaitGroup
	wg.Add(1)
	go deps.StreamRunLog(&wg, strings.NewReader(input), "run-1", "stderr", "warn", red)
	wg.Wait()

	logs, err := reg.GetRunLogs(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("GetRunLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("got %d log entries, want 2: %+v", len(logs), logs)
	}
	if logs[0].Level != "warn" || logs[1].Level != "warn" {
		t.Errorf("levels = %q/%q, want warn/warn", logs[0].Level, logs[1].Level)
	}
	if logs[0].Message != "hello" {
		t.Errorf("first line = %q, want %q", logs[0].Message, "hello")
	}
	if strings.Contains(logs[1].Message, "s3cret-value") {
		t.Errorf("secret leaked into run log: %q", logs[1].Message)
	}
}

// TestStreamRunLog_LongLine pins the issue #194 scanner sizing: a single
// line larger than the default bufio limit (64KiB) but under the configured
// 1MiB maximum must survive, not kill the stream.
func TestStreamRunLog_LongLine(t *testing.T) {
	deps, reg := newStreamTestDeps(t)
	red := secrets.NewRedactor(nil)

	long := strings.Repeat("x", 700*1024) // > 64KiB default, < 1MiB cap
	var wg sync.WaitGroup
	wg.Add(1)
	go deps.StreamRunLog(&wg, strings.NewReader(long+"\nafter\n"), "run-2", "stdout", "info", red)
	wg.Wait()

	logs, err := reg.GetRunLogs(context.Background(), "run-2")
	if err != nil {
		t.Fatalf("GetRunLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("got %d log entries, want 2 (long line must not abort the scan)", len(logs))
	}
	if len(logs[0].Message) != 700*1024 {
		t.Errorf("long line truncated: got %d bytes", len(logs[0].Message))
	}
	if logs[1].Message != "after" {
		t.Errorf("line after long line = %q, want %q", logs[1].Message, "after")
	}
}

// TestStreamRunLog_OversizedLineStopsStreamWithDiagnostic pins the
// scanner-error branch (issue #194's other half): a single line exceeding
// the 1 MiB scanner cap makes bufio return ErrTooLong — the stream stops,
// wg.Done still fires (no caller hang), and lines that arrived before the
// oversized one were already flushed.
func TestStreamRunLog_OversizedLineStopsStreamWithDiagnostic(t *testing.T) {
	deps, reg := newStreamTestDeps(t)
	red := secrets.NewRedactor(nil)

	input := "before\n" + strings.Repeat("x", 1024*1024+1) + "\nafter\n"
	var wg sync.WaitGroup
	wg.Add(1)
	go deps.StreamRunLog(&wg, strings.NewReader(input), "run-big", "stderr", "warn", red)
	wg.Wait() // must return despite the scanner error — callers block on this

	logs, err := reg.GetRunLogs(context.Background(), "run-big")
	if err != nil {
		t.Fatalf("GetRunLogs: %v", err)
	}
	if len(logs) != 1 || logs[0].Message != "before" {
		t.Fatalf("expected only the pre-error line to be flushed, got %d lines: %+v", len(logs), logs)
	}
}
