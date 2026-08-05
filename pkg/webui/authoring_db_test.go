package webui

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

func openTestDB(t *testing.T) db.DB {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestAuthoringSessionStore_CreateAndGet(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	now := time.Now()
	sess := AuthoringSession{
		ID:         "sess-1",
		Kind:       "create",
		Source:     "ai-scratch",
		TaskID:     "ai-scratch/my-task",
		CreatedAt:  now,
		LastTurnAt: now,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Kind != "create" {
		t.Errorf("Kind = %q, want %q", got.Kind, "create")
	}
	if got.Source != "ai-scratch" {
		t.Errorf("Source = %q, want %q", got.Source, "ai-scratch")
	}
	if got.TaskID != "ai-scratch/my-task" {
		t.Errorf("TaskID = %q, want %q", got.TaskID, "ai-scratch/my-task")
	}
	if got.ClosedAt != nil {
		t.Errorf("ClosedAt = %v, want nil", got.ClosedAt)
	}
	if got.Applied {
		t.Error("Applied = true, want false")
	}
}

func TestAuthoringSessionStore_GetNotFound(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	got, err := store.Get(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestAuthoringSessionStore_GetOpenForSource(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	now := time.Now()

	// Create an open session.
	sess := AuthoringSession{
		ID:         "sess-open",
		Kind:       "edit",
		Source:     "examples",
		TaskID:     "examples/hello",
		CreatedAt:  now,
		LastTurnAt: now,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Should find it.
	got, err := store.GetOpenForSource(ctx, "examples")
	if err != nil {
		t.Fatalf("GetOpenForSource: %v", err)
	}
	if got == nil {
		t.Fatal("expected open session, got nil")
	}
	if got.ID != "sess-open" {
		t.Errorf("ID = %q, want %q", got.ID, "sess-open")
	}

	// No open session for another source.
	got, err = store.GetOpenForSource(ctx, "ai-scratch")
	if err != nil {
		t.Fatalf("GetOpenForSource(other): %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for other source, got %+v", got)
	}
}

func TestAuthoringSessionStore_Close(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	now := time.Now()
	sess := AuthoringSession{
		ID:         "sess-close",
		Kind:       "create",
		Source:     "ai-scratch",
		CreatedAt:  now,
		LastTurnAt: now,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Close with applied=true.
	if err := store.Close(ctx, "sess-close", true); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := store.Get(ctx, "sess-close")
	if err != nil {
		t.Fatalf("Get after close: %v", err)
	}
	if got.ClosedAt == nil {
		t.Fatal("ClosedAt should be set after close")
	}
	if !got.Applied {
		t.Error("Applied should be true")
	}

	// GetOpenForSource should no longer find it.
	open, err := store.GetOpenForSource(ctx, "ai-scratch")
	if err != nil {
		t.Fatalf("GetOpenForSource: %v", err)
	}
	if open != nil {
		t.Errorf("expected nil after close, got %+v", open)
	}
}

func TestAuthoringSessionStore_ListOpen(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	now := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		sess := AuthoringSession{
			ID:         id,
			Kind:       "edit",
			Source:     "src-" + id,
			CreatedAt:  now,
			LastTurnAt: now,
		}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	// Close one.
	if err := store.Close(ctx, "b", false); err != nil {
		t.Fatalf("Close: %v", err)
	}

	open, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("ListOpen returned %d, want 2", len(open))
	}
}

func TestAuthoringSessionStore_PurgeExpired(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now()

	// One old session, one recent.
	for _, tc := range []struct {
		id string
		ts time.Time
	}{
		{"old", old},
		{"new", recent},
	} {
		sess := AuthoringSession{
			ID:         tc.id,
			Kind:       "edit",
			Source:     "src-" + tc.id,
			CreatedAt:  tc.ts,
			LastTurnAt: tc.ts,
		}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create %s: %v", tc.id, err)
		}
	}

	n, err := store.PurgeExpired(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("PurgeExpired returned %d, want 1", n)
	}

	// "old" should be closed now.
	got, err := store.Get(ctx, "old")
	if err != nil {
		t.Fatalf("Get old: %v", err)
	}
	if got.ClosedAt == nil {
		t.Error("old session should be closed after purge")
	}

	// "new" should still be open.
	got, err = store.Get(ctx, "new")
	if err != nil {
		t.Fatalf("Get new: %v", err)
	}
	if got.ClosedAt != nil {
		t.Error("new session should still be open")
	}
}

// TestAuthoringSessionStore_UpdateAgentSessionID pins the #568 multi-turn
// continuity mechanism: the agent's own session id starts unset, gets
// persisted after a turn, and is readable back on the next Get — which is
// exactly what pkg/ipc's handleTaskEdit relies on to continue the same
// ai-agent conversation across repeated `dicode task edit` calls.
func TestAuthoringSessionStore_UpdateAgentSessionID(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	now := time.Now()
	sess := AuthoringSession{
		ID:         "sess-agent",
		Kind:       "edit",
		Source:     "ai-scratch",
		CreatedAt:  now,
		LastTurnAt: now,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, "sess-agent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgentSessionID != nil {
		t.Fatalf("AgentSessionID = %v before any turn, want nil", got.AgentSessionID)
	}

	if err := store.UpdateAgentSessionID(ctx, "sess-agent", "asid-1"); err != nil {
		t.Fatalf("UpdateAgentSessionID: %v", err)
	}

	got, err = store.Get(ctx, "sess-agent")
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.AgentSessionID == nil || *got.AgentSessionID != "asid-1" {
		t.Errorf("AgentSessionID = %v, want asid-1", got.AgentSessionID)
	}

	// A second turn's session id overwrites the first.
	if err := store.UpdateAgentSessionID(ctx, "sess-agent", "asid-2"); err != nil {
		t.Fatalf("UpdateAgentSessionID (2nd): %v", err)
	}
	got, err = store.Get(ctx, "sess-agent")
	if err != nil {
		t.Fatalf("Get after 2nd update: %v", err)
	}
	if got.AgentSessionID == nil || *got.AgentSessionID != "asid-2" {
		t.Errorf("AgentSessionID after 2nd update = %v, want asid-2", got.AgentSessionID)
	}
}

// TestAuthoringSessionStore_UpdateAgentSessionID_BlankIsNoop asserts that an
// empty agentSessionID does not clobber a previously stored value — some
// alternative agent tasks may not return a session id on a given turn, and
// silently wiping continuity because of that would be a footgun.
func TestAuthoringSessionStore_UpdateAgentSessionID_BlankIsNoop(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	now := time.Now()
	sess := AuthoringSession{
		ID: "sess-blank", Kind: "edit", Source: "ai-scratch",
		CreatedAt: now, LastTurnAt: now,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.UpdateAgentSessionID(ctx, "sess-blank", "asid-1"); err != nil {
		t.Fatalf("UpdateAgentSessionID: %v", err)
	}
	if err := store.UpdateAgentSessionID(ctx, "sess-blank", ""); err != nil {
		t.Fatalf("UpdateAgentSessionID(blank): %v", err)
	}

	got, err := store.Get(ctx, "sess-blank")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgentSessionID == nil || *got.AgentSessionID != "asid-1" {
		t.Errorf("AgentSessionID = %v, want unchanged asid-1", got.AgentSessionID)
	}
}

func TestAuthoringSessionStore_UpdateLastTurn(t *testing.T) {
	d := openTestDB(t)
	store := newAuthoringSessionStore(d)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	sess := AuthoringSession{
		ID:         "sess-turn",
		Kind:       "edit",
		Source:     "ai-scratch",
		CreatedAt:  past,
		LastTurnAt: past,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.UpdateLastTurn(ctx, "sess-turn"); err != nil {
		t.Fatalf("UpdateLastTurn: %v", err)
	}

	got, err := store.Get(ctx, "sess-turn")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastTurnAt.Before(past.Add(30 * time.Minute)) {
		t.Errorf("LastTurnAt was not bumped: %v", got.LastTurnAt)
	}
}
