package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrResumeStateUnavailable is returned by ResumeStateStore.Fetch when the
// requested key has no stored blob (already GC'd, or the storage task
// returned an empty value for a missing key). Distinct from
// ErrInputUnavailable even though the underlying storage-task contract is
// identical, so callers can't accidentally conflate the two offload
// mechanisms' failure modes.
var ErrResumeStateUnavailable = errors.New("resume state unavailable (gc'd or never stored)")

// DefaultResumeStateThresholdBytes is the default cutoff above which a
// suspend's state blob is offloaded to the storage task instead of being
// written inline into runs.resume_state (#570).
//
// Chosen as 32 KiB: it is comfortably above a single LLM chat turn (a few KB
// of JSON) so the common case — a handful of turns — stays on the fast
// inline path, but well below the two limits that would otherwise bound it
// from above:
//   - the IPC frame cap (pkg/ipc.maxMessageSize, 8 MiB) that carries a
//     dicode.suspend() state blob from the task subprocess to the daemon —
//     32 KiB leaves three orders of magnitude of headroom before that limit
//     becomes a factor;
//   - InputCrypto's 64 MiB plaintext cap, which bounds the storage path this
//     offload reuses.
//
// The real motivation is the O(n²) SQLite cost the issue describes: a
// cumulative LLM-context state is re-persisted in full on every suspend, so
// even a moderate per-turn size compounds badly over a long conversation.
// 32 KiB keeps that inline-rewrite cost bounded to short conversations only;
// anything that would grow the `runs` row unboundedly offloads instead.
const DefaultResumeStateThresholdBytes = 32 * 1024

// resumeStateKeyPrefix namespaces resume-state storage keys apart from
// run-input keys ("run-inputs/") even though both mechanisms may share the
// same storage task — see ResumeStateStore's doc comment.
const resumeStateKeyPrefix = "resume-state/"

// ResumeStateStore offloads large dicode.suspend() state blobs to a
// configured storage task, mirroring InputStore's marshal → encrypt →
// delegate-to-storage-task shape but deliberately kept as a separate type
// rather than a shared implementation:
//
//   - distinct encryption sub-key (callers construct it from
//     "dicode/resume-state/v1", never "dicode/run-inputs/v1") so a bug in one
//     mechanism can't decrypt the other's blobs;
//   - distinct storage-key prefix ("resume-state/" vs "run-inputs/") AND
//     distinct root/prefix params passed explicitly on every call (not the
//     storage task's own run-inputs-shaped defaults), so the two mechanisms
//     write to different directories on disk and can never collide even if
//     both happen to key off the same run ID;
//   - distinct GC lifetime — a resume-state blob is dead weight the moment
//     its row leaves `suspended` (see Registry.ListExpiredResumeStates),
//     whereas a run-input blob is an audit trail kept for its own retention
//     window regardless of the run's status.
type ResumeStateStore struct {
	crypto      *InputCrypto
	runner      TaskRunner
	storageTask string
	root        string // explicit storage root, e.g. "${DATADIR}/resume-state" pre-resolved by the caller
}

// NewResumeStateStore constructs a ResumeStateStore. crypto must be
// initialised with a 32-byte sub-key distinct from InputStore's (typically
// secrets.LocalProvider.DeriveSubKey("dicode/resume-state/v1")). root is the
// resolved filesystem root passed to the storage task's "root" param on every
// call (e.g. filepath.Join(dataDir, "resume-state")) — passed explicitly
// because the storage task's own default root/prefix are shaped for
// run-inputs, not resume-state.
func NewResumeStateStore(crypto *InputCrypto, runner TaskRunner, storageTask, root string) *ResumeStateStore {
	return &ResumeStateStore{crypto: crypto, runner: runner, storageTask: storageTask, root: root}
}

// Persist marshals + encrypts + stores a raw resume-state blob, keyed by the
// suspending run's own ID (so AAD binding and the storage key line up with
// what Fetch is called with later — the same convention InputStore uses).
// Returns the storage key, ciphertext byte size, and stored_at unix
// timestamp the AAD was bound to (caller persists these on the runs row via
// SuspendRun's blobRef).
func (s *ResumeStateStore) Persist(ctx context.Context, runID string, state []byte) (key string, size int, storedAt int64, err error) {
	storedAt = timeNow().Unix()
	blob, err := s.crypto.Encrypt(state, runID, storedAt)
	if err != nil {
		return "", 0, 0, fmt.Errorf("encrypt: %w", err)
	}
	key = resumeStateKeyPrefix + runID
	enc := base64.StdEncoding.EncodeToString(blob)
	if _, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":     "put",
		"key":    key,
		"value":  enc,
		"root":   s.root,
		"prefix": resumeStateKeyPrefix,
	}); err != nil {
		return "", 0, 0, fmt.Errorf("storage put: %w", err)
	}
	return key, len(blob), storedAt, nil
}

// Fetch retrieves and decrypts a previously-persisted resume-state blob.
// runID and storedAt must match what Persist was called with (they're bound
// into the AEAD's AAD) — for resume-state this is always the suspended run's
// own ID/stored_at, read back off its runs row.
func (s *ResumeStateStore) Fetch(ctx context.Context, runID, key string, storedAt int64) ([]byte, error) {
	res, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":     "get",
		"key":    key,
		"root":   s.root,
		"prefix": resumeStateKeyPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("storage get: %w", err)
	}
	resMap, ok := res.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("storage task returned non-map: %T", res)
	}
	encStr, _ := resMap["value"].(string)
	if encStr == "" {
		return nil, ErrResumeStateUnavailable
	}
	blob, err := base64.StdEncoding.DecodeString(encStr)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	pt, err := s.crypto.Decrypt(blob, runID, storedAt)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return pt, nil
}

// Delete removes the stored blob via the configured storage task. Idempotent
// at the contract level — the storage task does not error on missing keys.
func (s *ResumeStateStore) Delete(ctx context.Context, key string) error {
	_, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":     "delete",
		"key":    key,
		"root":   s.root,
		"prefix": resumeStateKeyPrefix,
	})
	if err != nil {
		return fmt.Errorf("storage delete: %w", err)
	}
	return nil
}
