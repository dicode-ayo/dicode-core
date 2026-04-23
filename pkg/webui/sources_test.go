package webui

import (
	"errors"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// TestSourceManager_List_PopulatesPullStatus guards the plumbing from
// taskset.Source.PullStatus() → mcp.SourceEntry so the frontend can
// render a per-source health dot (#87).
func TestSourceManager_List_PopulatesPullStatus(t *testing.T) {
	cfg := &config.Config{
		Sources: []config.SourceConfig{
			{Type: config.SourceTypeGit, URL: "https://example.test/repo", Branch: "main"},
		},
	}

	// A taskset.Source with a recorded failing pull. Built via NewSource
	// so internal fields (resolver, log) are populated; we don't run
	// Start so no network access happens.
	src := taskset.NewSource(
		"https://example.test/repo",
		"buildin",
		&taskset.Ref{URL: "https://example.test/repo", Branch: "main"},
		"",
		t.TempDir(),
		false,
		0,
		zap.NewNop(),
	)
	want := errors.New("pull: object not found")
	before := time.Now()
	src.SetPullStatusForTest(want)

	m := NewSourceManager(cfg, map[string]*taskset.Source{
		sourceName(cfg.Sources[0]): src,
	}, t.TempDir(), zap.NewNop())

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("len(List()) = %d; want 1", len(list))
	}
	got := list[0]
	if got.LastPullOK {
		t.Error("LastPullOK should be false after a failed pull")
	}
	if got.LastPullError != want.Error() {
		t.Errorf("LastPullError = %q; want %q", got.LastPullError, want.Error())
	}
	if got.LastPullAt.Before(before) {
		t.Errorf("LastPullAt = %v; should be >= %v", got.LastPullAt, before)
	}
}

// TestSourceManager_List_LocalSource_NoPullStatus verifies local sources
// leave the pull fields zero so the UI knows to skip the dot.
func TestSourceManager_List_LocalSource_NoPullStatus(t *testing.T) {
	cfg := &config.Config{
		Sources: []config.SourceConfig{
			{Type: config.SourceTypeLocal, Path: "/tmp/tasks"},
		},
	}
	m := NewSourceManager(cfg, nil, t.TempDir(), zap.NewNop())

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("len(List()) = %d; want 1", len(list))
	}
	if !list[0].LastPullAt.IsZero() {
		t.Errorf("local source should have zero LastPullAt; got %v", list[0].LastPullAt)
	}
	if list[0].LastPullError != "" {
		t.Errorf("local source should have empty LastPullError; got %q", list[0].LastPullError)
	}
}
