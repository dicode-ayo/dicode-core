package audit

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return NewStore(d)
}

func TestStore_AppendAndQuery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	events := []Event{
		{EventType: EventRunTriggered, ActorKind: "cron", ActorID: "", TargetKind: "task", TargetID: "ns/task-a", RunID: "run-1", Allowed: true},
		{EventType: EventTaskCalled, ActorKind: "task", ActorID: "ns/caller", TargetKind: "task", TargetID: "ns/task-b", RunID: "run-2", Allowed: true},
		{EventType: EventTaskCalled, ActorKind: "task", ActorID: "ns/caller", TargetKind: "task", TargetID: "ns/task-c", Allowed: false, Reason: "not in security.allowed_tasks"},
		{EventType: EventDenied, ActorKind: "ip", ActorID: "10.0.0.7", TargetKind: "endpoint", TargetID: "GET /api/tasks", Allowed: false, Reason: "unauthorized"},
	}
	for i, ev := range events {
		// Spread timestamps so ordering is deterministic.
		ev.TS = time.Date(2026, 6, 1, 12, 0, i, 0, time.UTC)
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	// Unfiltered: newest first.
	all, err := s.Query(ctx, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d events, want 4", len(all))
	}
	if all[0].EventType != EventDenied || all[3].EventType != EventRunTriggered {
		t.Errorf("expected newest-first ordering, got %s … %s", all[0].EventType, all[3].EventType)
	}
	if all[0].Allowed {
		t.Error("denied event must have Allowed=false")
	}
	if all[0].ID == "" || all[0].TS.IsZero() {
		t.Error("Append must fill ID and TS")
	}

	// Filter by task (target_id).
	byTask, err := s.Query(ctx, Filter{TaskID: "ns/task-b"})
	if err != nil {
		t.Fatalf("Query by task: %v", err)
	}
	if len(byTask) != 1 || byTask[0].RunID != "run-2" {
		t.Errorf("task filter: got %+v", byTask)
	}

	// Filter by actor.
	byActor, err := s.Query(ctx, Filter{Actor: "ns/caller"})
	if err != nil {
		t.Fatalf("Query by actor: %v", err)
	}
	if len(byActor) != 2 {
		t.Errorf("actor filter: got %d events, want 2", len(byActor))
	}

	// Filter by event type.
	byType, err := s.Query(ctx, Filter{EventType: EventDenied})
	if err != nil {
		t.Fatalf("Query by type: %v", err)
	}
	if len(byType) != 1 || byType[0].ActorID != "10.0.0.7" {
		t.Errorf("event_type filter: got %+v", byType)
	}

	// Combined filters.
	combined, err := s.Query(ctx, Filter{Actor: "ns/caller", TaskID: "ns/task-c"})
	if err != nil {
		t.Fatalf("Query combined: %v", err)
	}
	if len(combined) != 1 || combined[0].Reason != "not in security.allowed_tasks" {
		t.Errorf("combined filter: got %+v", combined)
	}
}

func TestStore_QueryPagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		err := s.Append(ctx, Event{
			EventType: EventRunTriggered,
			TargetID:  "t",
			TS:        time.Date(2026, 6, 1, 12, 0, i, 0, time.UTC),
			Allowed:   true,
		})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	page1, err := s.Query(ctx, Filter{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	page2, err := s.Query(ctx, Filter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("page sizes: %d, %d, want 2, 2", len(page1), len(page2))
	}
	if page1[0].ID == page2[0].ID || page1[1].ID == page2[0].ID {
		t.Error("pages overlap")
	}
	if !page1[1].TS.After(page2[0].TS) && !page1[1].TS.Equal(page2[0].TS) {
		t.Errorf("expected page1 to be newer than page2: %v vs %v", page1[1].TS, page2[0].TS)
	}
}

func TestStore_Prune(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	old := Event{EventType: EventDenied, TargetID: "old", TS: time.Now().UTC().AddDate(0, 0, -45)}
	fresh := Event{EventType: EventDenied, TargetID: "fresh", TS: time.Now().UTC()}
	if err := s.Append(ctx, old); err != nil {
		t.Fatalf("append old: %v", err)
	}
	if err := s.Append(ctx, fresh); err != nil {
		t.Fatalf("append fresh: %v", err)
	}

	// retentionDays <= 0 must be a no-op (never wipes the table).
	if err := s.Prune(ctx, 0); err != nil {
		t.Fatalf("Prune(0): %v", err)
	}
	if err := s.Prune(ctx, -3); err != nil {
		t.Fatalf("Prune(-3): %v", err)
	}
	all, _ := s.Query(ctx, Filter{})
	if len(all) != 2 {
		t.Fatalf("Prune(0/-3) must not delete anything: %d rows left", len(all))
	}

	if err := s.Prune(ctx, 30); err != nil {
		t.Fatalf("Prune(30): %v", err)
	}
	after, _ := s.Query(ctx, Filter{})
	if len(after) != 1 || after[0].TargetID != "fresh" {
		t.Fatalf("expected only the fresh row to survive, got %+v", after)
	}
}

// TestStore_QueryDescContractUnchanged regression-guards the #45 behaviour:
// with no cursor and no Ascending flag, Query returns newest-first and honours
// limit/offset exactly as before #415.
func TestStore_QueryDescContractUnchanged(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.Append(ctx, Event{
			EventType: EventRunTriggered, TargetID: "t",
			TS: time.Date(2026, 6, 1, 12, 0, i, 0, time.UTC), Allowed: true,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	all, err := s.Query(ctx, Filter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("got %d, want 5", len(all))
	}
	// Newest first: index 0 is the latest timestamp.
	for i := 0; i+1 < len(all); i++ {
		if all[i].TS.Before(all[i+1].TS) {
			t.Errorf("not newest-first at %d: %v before %v", i, all[i].TS, all[i+1].TS)
		}
	}
	page1, _ := s.Query(ctx, Filter{Limit: 2})
	page2, _ := s.Query(ctx, Filter{Limit: 2, Offset: 2})
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("offset paging broke: %d, %d", len(page1), len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Error("offset paging overlap")
	}
}

// TestStore_CursorExactlyOnceUnderConcurrentWriter is the #415 acceptance
// test: page ascending with a cursor, append more rows mid-paging, resume —
// no duplicates, no gaps. Offset paging would dupe/skip here because inserts
// shift the window; the (ts, id) cursor does not.
func TestStore_CursorExactlyOnceUnderConcurrentWriter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	appendAt := func(sec int) {
		if err := s.Append(ctx, Event{
			EventType: EventRunTriggered, TargetID: "t",
			TS: time.Date(2026, 6, 1, 12, 0, sec, 0, time.UTC), Allowed: true,
		}); err != nil {
			t.Fatalf("append @%d: %v", sec, err)
		}
	}

	// Initial 3 rows.
	for i := 0; i < 3; i++ {
		appendAt(i)
	}

	seen := map[string]bool{}
	var cursor Cursor

	// Page 1 (ascending, limit 2).
	p1, err := s.Query(ctx, Filter{Ascending: true, Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(p1) != 2 {
		t.Fatalf("page1 size %d, want 2", len(p1))
	}
	for _, ev := range p1 {
		seen[ev.ID] = true
	}
	cursor = CursorOf(p1[len(p1)-1])

	// Concurrent writer appends 2 more rows AFTER the cursor position.
	appendAt(3)
	appendAt(4)

	// Resume from the cursor — must pick up the 1 leftover original plus the
	// 2 new rows, with no row seen twice.
	for {
		page, err := s.Query(ctx, Filter{Ascending: true, Limit: 2, After: cursor})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			if seen[ev.ID] {
				t.Errorf("duplicate event id across cursor pages: %s", ev.ID)
			}
			seen[ev.ID] = true
		}
		cursor = CursorOf(page[len(page)-1])
	}

	if len(seen) != 5 {
		t.Errorf("expected 5 distinct events (3 original + 2 concurrent), got %d", len(seen))
	}
}

// TestStore_CursorOffsetIgnoredWhenAfterSet confirms Offset is not applied
// alongside a cursor (the two are mutually exclusive).
func TestStore_CursorOffsetIgnoredWhenAfterSet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := s.Append(ctx, Event{
			EventType: EventRunTriggered, TargetID: "t",
			TS: time.Date(2026, 6, 1, 12, 0, i, 0, time.UTC), Allowed: true,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	first, _ := s.Query(ctx, Filter{Ascending: true, Limit: 1})
	cursor := CursorOf(first[0])
	// With a stale Offset that would skip everything if applied, the cursor
	// must still return the 3 rows after `first`.
	rest, err := s.Query(ctx, Filter{Ascending: true, After: cursor, Offset: 100})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rest) != 3 {
		t.Errorf("offset leaked into cursor paging: got %d rows, want 3", len(rest))
	}
}

func TestCursor_EncodeDecodeRoundTrip(t *testing.T) {
	orig := Cursor{TS: time.Date(2026, 6, 1, 12, 30, 45, 123_000_000, time.UTC), ID: "abc-123"}
	tok := EncodeCursor(orig)
	if tok == "" {
		t.Fatal("non-zero cursor must encode to a non-empty token")
	}
	got, err := DecodeCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != orig.ID || !got.TS.Equal(orig.TS) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, orig)
	}

	// Empty token ↔ zero cursor.
	if EncodeCursor(Cursor{}) != "" {
		t.Error("zero cursor must encode to empty string")
	}
	z, err := DecodeCursor("")
	if err != nil || !z.IsZero() {
		t.Errorf("empty token must decode to zero cursor: %+v, %v", z, err)
	}

	// Malformed tokens are an error.
	if _, err := DecodeCursor("!!!not-base64!!!"); err == nil {
		t.Error("expected error for malformed base64 cursor")
	}
	noSep := base64.RawURLEncoding.EncodeToString([]byte("no-separator"))
	if _, err := DecodeCursor(noSep); err == nil {
		t.Error("expected error for cursor missing separator")
	}
}

func TestStore_NilSafe(t *testing.T) {
	var s *Store
	ctx := context.Background()
	if err := s.Append(ctx, Event{EventType: EventDenied}); err != nil {
		t.Errorf("nil store Append: %v", err)
	}
	s.Emit(ctx, Event{EventType: EventDenied}) // must not panic
	evs, err := s.Query(ctx, Filter{})
	if err != nil || len(evs) != 0 {
		t.Errorf("nil store Query: %v, %v", evs, err)
	}
	if err := s.Prune(ctx, 30); err != nil {
		t.Errorf("nil store Prune: %v", err)
	}
	if NewStore(nil) != nil {
		t.Error("NewStore(nil) must return nil")
	}
}
