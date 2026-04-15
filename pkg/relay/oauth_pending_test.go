package relay

import (
	"errors"
	"testing"
	"time"
)

func newAuthReq(sessionID string, ts int64) *AuthRequest {
	return &AuthRequest{
		Provider:  "slack",
		SessionID: sessionID,
		Timestamp: ts,
	}
}

func TestPendingSessions_AddTake(t *testing.T) {
	store := NewPendingSessions()
	store.Add(newAuthReq("s-1", time.Now().Unix()))
	if store.Len() != 1 {
		t.Fatalf("len: %d", store.Len())
	}

	got, err := store.Take("s-1")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if got.SessionID != "s-1" || got.Provider != "slack" {
		t.Fatalf("bad take: %+v", got)
	}
	if store.Len() != 0 {
		t.Fatalf("take should evict")
	}
}

func TestPendingSessions_TakeMissing(t *testing.T) {
	store := NewPendingSessions()
	if _, err := store.Take("nope"); !errors.Is(err, ErrPendingNotFound) {
		t.Fatalf("expected ErrPendingNotFound, got %v", err)
	}
}

func TestPendingSessions_TakeExpired(t *testing.T) {
	store := NewPendingSessions()
	frozen := time.Now()
	store.nowFunc = func() time.Time { return frozen }
	store.ttl = time.Second

	// Timestamp the session as 10s ago; nowFunc is frozen at 'now'.
	store.Add(newAuthReq("s-old", frozen.Add(-10*time.Second).Unix()))
	if _, err := store.Take("s-old"); !errors.Is(err, ErrPendingNotFound) {
		t.Fatalf("expected expired → ErrPendingNotFound, got %v", err)
	}
}

func TestPendingSessions_Sweep(t *testing.T) {
	store := NewPendingSessions()
	frozen := time.Now()
	store.nowFunc = func() time.Time { return frozen }
	store.ttl = time.Minute

	store.Add(newAuthReq("fresh", frozen.Unix()))
	store.Add(newAuthReq("stale", frozen.Add(-2*time.Minute).Unix()))
	if removed := store.SweepExpired(); removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if store.Len() != 1 {
		t.Fatalf("len after sweep: %d", store.Len())
	}
}
