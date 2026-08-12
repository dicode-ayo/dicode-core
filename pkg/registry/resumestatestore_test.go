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

// notOKRunner simulates buildin/local-storage catching its own internal
// failure and returning it as a normal {"ok": false, "error": "..."}
// payload — a nil Go error from RunTaskSync — rather than an uncaught
// exception. Reproduces tasks/buildin/local-storage/task.ts's actual
// contract: every branch in its outer try/catch returns, never throws.
type notOKRunner struct{ msg string }

func (r *notOKRunner) RunTaskSync(ctx context.Context, taskID string, params map[string]string) (any, error) {
	return map[string]any{"ok": false, "error": r.msg}, nil
}

// TestResumeStateStore_PersistRejectsOKFalse locks in a bug found during
// code review: Persist checked only RunTaskSync's Go error, not the
// storage task's own "ok" field. A put the storage task caught and reported
// as failed (disk full, permission denied, ...) was returned to the caller
// as nil error with a real key/size — suspendRun then landed the run
// StatusSuspended with resume_state_storage_key pointing at a blob that was
// never actually written, exactly the dangling-reference case #570 exists
// to prevent. Before this fix, this test's Persist call returned err == nil.
func TestResumeStateStore_PersistRejectsOKFalse(t *testing.T) {
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, &notOKRunner{msg: "disk full"}, "any-storage", "/data/resume-state")
	runID := uuid.New().String()
	if _, _, _, err := s.Persist(context.Background(), runID, []byte(`{}`)); err == nil {
		t.Fatal("expected Persist to fail when the storage task reports ok:false")
	}
}

// TestResumeStateStore_FetchRejectsOKFalse is Fetch's counterpart: an
// ok:false get response (as opposed to the legitimate ok:true,value:""
// missing-key case) must not be silently treated as "unavailable" — it's a
// real storage-task failure, not an absent blob, and conflating the two
// would surface the wrong error to an operator debugging a stuck resume.
func TestResumeStateStore_FetchRejectsOKFalse(t *testing.T) {
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, &notOKRunner{msg: "permission denied"}, "any-storage", "/data/resume-state")
	runID := uuid.New().String()
	_, err := s.Fetch(context.Background(), runID, "resume-state/"+runID, time.Now().Unix())
	if err == nil {
		t.Fatal("expected Fetch to fail when the storage task reports ok:false")
	}
	if errors.Is(err, ErrResumeStateUnavailable) {
		t.Error("an ok:false failure must not be reported as ErrResumeStateUnavailable (a real storage error, not a missing key)")
	}
}

// TestResumeStateStore_DeleteRejectsOKFalse is Delete's counterpart: a
// delete the storage task caught and reported as failed must not be treated
// as a successful GC — silently believing the blob is gone leaves it
// permanently orphaned on disk with no DB reference to ever find it again.
func TestResumeStateStore_DeleteRejectsOKFalse(t *testing.T) {
	c := newTestResumeStateCrypto(t)
	s := NewResumeStateStore(c, &notOKRunner{msg: "permission denied"}, "any-storage", "/data/resume-state")
	if err := s.Delete(context.Background(), "resume-state/x"); err == nil {
		t.Fatal("expected Delete to fail when the storage task reports ok:false")
	}
}
