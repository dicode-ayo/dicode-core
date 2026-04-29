package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// mockRunner is an in-memory storage backend for testing. It conforms to
// TaskRunner but simulates a local-storage task: put/get/delete on a
// map[string]string of base64-encoded blobs.
type mockRunner struct {
	store map[string]string
}

func (m *mockRunner) RunTaskSync(ctx context.Context, taskID string, params map[string]string) (any, error) {
	switch params["op"] {
	case "put":
		m.store[params["key"]] = params["value"]
		return map[string]any{"ok": true}, nil
	case "get":
		v, ok := m.store[params["key"]]
		if !ok {
			return map[string]any{"ok": true, "value": ""}, nil
		}
		return map[string]any{"ok": true, "value": v}, nil
	case "delete":
		delete(m.store, params["key"])
		return map[string]any{"ok": true}, nil
	}
	return nil, errors.New("unknown op")
}

func TestInputStore_RoundTrip(t *testing.T) {
	frozen := time.Unix(1714400000, 0)
	prev := timeNow
	timeNow = func() time.Time { return frozen }
	defer func() { timeNow = prev }()

	mr := &mockRunner{store: map[string]string{}}
	c := newTestInputCrypto(t)
	s := NewInputStore(c, mr, "buildin/local-storage")

	runID := uuid.New().String()
	in := PersistedInput{Source: "webhook", Method: "POST", Path: "/hooks/x"}

	key, size, storedAt, err := s.Persist(context.Background(), runID, in)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Errorf("size = %d, want > 0", size)
	}
	if storedAt != frozen.Unix() {
		t.Errorf("storedAt = %d, want %d", storedAt, frozen.Unix())
	}
	if key == "" {
		t.Error("key empty")
	}
	// Confirm the stored value is base64.
	if _, err := base64.StdEncoding.DecodeString(mr.store[key]); err != nil {
		t.Errorf("stored value is not base64: %v", err)
	}

	// Fetch round-trips.
	got, err := s.Fetch(context.Background(), runID, key, storedAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "webhook" || got.Method != "POST" || got.Path != "/hooks/x" {
		t.Errorf("got = %#v", got)
	}

	// Delete + fetch returns ErrInputUnavailable.
	if err := s.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), runID, key, storedAt); !errors.Is(err, ErrInputUnavailable) {
		t.Errorf("expected ErrInputUnavailable after delete; got %v", err)
	}
}

func TestInputStore_StoredBlobIsCiphertext(t *testing.T) {
	mr := &mockRunner{store: map[string]string{}}
	c := newTestInputCrypto(t)
	s := NewInputStore(c, mr, "any-storage")

	runID := uuid.New().String()
	plaintextMarker := "VERY_SENSITIVE_MARKER"
	in := PersistedInput{Source: "webhook", Path: plaintextMarker}

	key, _, _, err := s.Persist(context.Background(), runID, in)
	if err != nil {
		t.Fatal(err)
	}
	stored := mr.store[key]
	// The base64-decoded blob must NOT contain the plaintext marker.
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+len(plaintextMarker) <= len(raw); i++ {
		if string(raw[i:i+len(plaintextMarker)]) == plaintextMarker {
			t.Fatal("plaintext leaked into stored blob")
		}
	}
}

func TestInputStore_FetchUnknownKeyReturnsErrInputUnavailable(t *testing.T) {
	mr := &mockRunner{store: map[string]string{}}
	c := newTestInputCrypto(t)
	s := NewInputStore(c, mr, "any-storage")

	runID := uuid.New().String()
	_, err := s.Fetch(context.Background(), runID, "missing-key", time.Now().Unix())
	if !errors.Is(err, ErrInputUnavailable) {
		t.Errorf("expected ErrInputUnavailable; got %v", err)
	}
}

func TestInputStore_FetchWithWrongRunIDFails(t *testing.T) {
	mr := &mockRunner{store: map[string]string{}}
	c := newTestInputCrypto(t)
	s := NewInputStore(c, mr, "any-storage")

	runA := uuid.New().String()
	runB := uuid.New().String()
	in := PersistedInput{Source: "webhook"}

	key, _, storedAt, err := s.Persist(context.Background(), runA, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), runB, key, storedAt); err == nil {
		t.Error("expected decrypt failure when fetching with wrong runID")
	}
}

type errRunner struct{ err error }

func (r *errRunner) RunTaskSync(ctx context.Context, taskID string, params map[string]string) (any, error) {
	return nil, r.err
}

func TestInputStore_PersistPropagatesRunnerError(t *testing.T) {
	c := newTestInputCrypto(t)
	s := NewInputStore(c, &errRunner{err: errors.New("storage backend down")}, "any-storage")
	runID := uuid.New().String()
	if _, _, _, err := s.Persist(context.Background(), runID, PersistedInput{}); err == nil {
		t.Error("expected error from runner to propagate")
	}
}
