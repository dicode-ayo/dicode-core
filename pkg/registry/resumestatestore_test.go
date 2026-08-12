package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newTestResumeStateCrypto returns an InputCrypto seeded with a DIFFERENT
// fixed test key than newTestInputCrypto, mirroring the real deployment
// where ResumeStateStore and InputStore are derived from distinct sub-keys
// ("dicode/resume-state/v1" vs "dicode/run-inputs/v1") so a blob encrypted
// under one can never be decrypted under the other.
func newTestResumeStateCrypto(t *testing.T) *InputCrypto {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}
	return NewInputCrypto(key)
}

func TestResumeStateStore_RoundTrip(t *testing.T) {
	frozen := time.Unix(1714400000, 0)
	prev := timeNow
	timeNow = func() time.Time { return frozen }
	defer func() { timeNow = prev }()

	mr := &mockRunner{store: map[string]string{}}
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, mr, "buildin/local-storage", "/data/resume-state")

	runID := uuid.New().String()
	state := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)

	key, size, storedAt, err := s.Persist(context.Background(), runID, state)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Errorf("size = %d, want > 0", size)
	}
	if storedAt != frozen.Unix() {
		t.Errorf("storedAt = %d, want %d", storedAt, frozen.Unix())
	}
	if key != "resume-state/"+runID {
		t.Errorf("key = %q, want %q", key, "resume-state/"+runID)
	}
	// Persist must pass the dedicated root/prefix explicitly rather than
	// relying on the storage task's run-inputs-shaped defaults.
	if mr.lastParams["root"] != "/data/resume-state" {
		t.Errorf("root param = %q, want /data/resume-state", mr.lastParams["root"])
	}
	if mr.lastParams["prefix"] != "resume-state/" {
		t.Errorf("prefix param = %q, want resume-state/", mr.lastParams["prefix"])
	}
	if _, err := base64.StdEncoding.DecodeString(mr.store[key]); err != nil {
		t.Errorf("stored value is not base64: %v", err)
	}

	got, err := s.Fetch(context.Background(), runID, key, storedAt)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(state) {
		t.Errorf("got = %q, want %q", got, state)
	}

	if err := s.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), runID, key, storedAt); !errors.Is(err, ErrResumeStateUnavailable) {
		t.Errorf("expected ErrResumeStateUnavailable after delete; got %v", err)
	}
}

func TestResumeStateStore_StoredBlobIsCiphertext(t *testing.T) {
	mr := &mockRunner{store: map[string]string{}}
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, mr, "any-storage", "/data/resume-state")

	runID := uuid.New().String()
	plaintextMarker := "VERY_SENSITIVE_CONVERSATION_TURN"
	state := []byte(`{"note":"` + plaintextMarker + `"}`)

	key, _, _, err := s.Persist(context.Background(), runID, state)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(mr.store[key])
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+len(plaintextMarker) <= len(raw); i++ {
		if string(raw[i:i+len(plaintextMarker)]) == plaintextMarker {
			t.Fatal("plaintext leaked into stored blob")
		}
	}
}

func TestResumeStateStore_KeyPrefixDistinctFromInputStore(t *testing.T) {
	mr := &mockRunner{store: map[string]string{}}
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, mr, "buildin/local-storage", "/data/resume-state")

	runID := uuid.New().String()
	key, _, _, err := s.Persist(context.Background(), runID, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if key == "run-inputs/"+runID {
		t.Errorf("resume-state key collided with the run-inputs key shape: %q", key)
	}
}

func TestResumeStateStore_FetchUnknownKeyReturnsErrResumeStateUnavailable(t *testing.T) {
	mr := &mockRunner{store: map[string]string{}}
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, mr, "any-storage", "/data/resume-state")

	runID := uuid.New().String()
	if _, err := s.Fetch(context.Background(), runID, "missing-key", time.Now().Unix()); !errors.Is(err, ErrResumeStateUnavailable) {
		t.Errorf("expected ErrResumeStateUnavailable; got %v", err)
	}
}

func TestResumeStateStore_FetchWithWrongRunIDFails(t *testing.T) {
	mr := &mockRunner{store: map[string]string{}}
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, mr, "any-storage", "/data/resume-state")

	runA := uuid.New().String()
	runB := uuid.New().String()

	key, _, storedAt, err := s.Persist(context.Background(), runA, []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), runB, key, storedAt); err == nil {
		t.Error("expected decrypt failure when fetching with wrong runID")
	}
}

func TestResumeStateStore_PersistPropagatesRunnerError(t *testing.T) {
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, &errRunner{err: errors.New("storage backend down")}, "any-storage", "/data/resume-state")
	runID := uuid.New().String()
	if _, _, _, err := s.Persist(context.Background(), runID, []byte(`{}`)); err == nil {
		t.Error("expected error from runner to propagate")
	}
}
