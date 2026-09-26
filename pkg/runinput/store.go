// Package runinput owns what a run was given: building the persisted record
// from a trigger's params, body and webhook context, redacting the credentials
// out of it, encrypting it, handing the bytes to the configured storage task,
// and replaying a stored input back through the engine.
//
// The run log in pkg/registry holds only the scalar columns that address a
// stored input — its key, size, stored_at and the names redacted out of it —
// so the two packages meet at the runs table and nowhere else.
package runinput

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TaskRunner abstracts the trigger engine's ability to invoke a task
// synchronously with given string params. Used by Store to delegate
// byte storage to a configured storage task without taking a hard
// dependency on pkg/trigger.
type TaskRunner interface {
	RunTaskSync(ctx context.Context, taskID string, params map[string]string) (any, error)
}

// ErrUnavailable is returned by Fetch when the requested key has no
// stored blob. Callers (e.g. replay in #234) treat this as a signal to
// abort the operation rather than retry.
var ErrUnavailable = errors.New("run input unavailable (gc'd or never stored)")

// ErrStorageTaskNotRegistered wraps Persist/Fetch/Delete failures that stem
// purely from the configured storage task not yet being in the registry.
// Daemon-triggered runs can fire before buildin/local-storage registers at
// startup (#523); callers use errors.Is to treat this transient window
// distinctly from a genuine storage write failure.
var ErrStorageTaskNotRegistered = errors.New("storage task not registered")

// Store marshals Persisted, encrypts it with Crypto, and
// delegates byte storage to a configured storage task via a TaskRunner.
//
// The storage task is identified by its task ID (e.g. "buildin/local-storage").
// Its contract is: accept op=put|get|delete with key + optional value
// params, return a map with at minimum {"ok": bool}; "value" is required
// for get when found.
type Store struct {
	crypto      *Crypto
	runner      TaskRunner
	storageTask string
}

// NewStore constructs a Store. The crypto must be initialized
// with a 32-byte sub-key (typically secrets.LocalProvider.DeriveSubKey
// "dicode/run-inputs/v1").
func NewStore(crypto *Crypto, runner TaskRunner, storageTask string) *Store {
	return &Store{crypto: crypto, runner: runner, storageTask: storageTask}
}

// Persist marshals + encrypts + stores. Returns the storage key, the
// ciphertext byte size, and the stored_at unix timestamp the AAD was
// bound to (caller stores this on the runs row).
func (s *Store) Persist(ctx context.Context, runID string, in Persisted) (key string, size int, storedAt int64, err error) {
	pt, err := json.Marshal(in)
	if err != nil {
		return "", 0, 0, fmt.Errorf("marshal: %w", err)
	}
	storedAt = timeNow().Unix()
	blob, err := s.crypto.Encrypt(pt, runID, storedAt)
	if err != nil {
		return "", 0, 0, fmt.Errorf("encrypt: %w", err)
	}
	key = "run-inputs/" + runID
	enc := base64.StdEncoding.EncodeToString(blob)
	if _, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":    "put",
		"key":   key,
		"value": enc,
	}); err != nil {
		return "", 0, 0, fmt.Errorf("storage put: %w", err)
	}
	return key, len(blob), storedAt, nil
}

// Fetch retrieves and decrypts a previously-persisted input.
func (s *Store) Fetch(ctx context.Context, runID, key string, storedAt int64) (Persisted, error) {
	res, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":  "get",
		"key": key,
	})
	if err != nil {
		return Persisted{}, fmt.Errorf("storage get: %w", err)
	}
	resMap, ok := res.(map[string]any)
	if !ok {
		return Persisted{}, fmt.Errorf("storage task returned non-map: %T", res)
	}
	encStr, _ := resMap["value"].(string)
	if encStr == "" {
		return Persisted{}, ErrUnavailable
	}
	blob, err := base64.StdEncoding.DecodeString(encStr)
	if err != nil {
		return Persisted{}, fmt.Errorf("decode: %w", err)
	}
	pt, err := s.crypto.Decrypt(blob, runID, storedAt)
	if err != nil {
		return Persisted{}, fmt.Errorf("decrypt: %w", err)
	}
	var out Persisted
	if err := json.Unmarshal(pt, &out); err != nil {
		return Persisted{}, fmt.Errorf("unmarshal: %w", err)
	}
	return out, nil
}

// Delete removes the stored blob via the configured storage task.
// Idempotent at the contract level — the storage task should not error on
// missing keys, but if it does, the error is returned.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":  "delete",
		"key": key,
	})
	if err != nil {
		return fmt.Errorf("storage delete: %w", err)
	}
	return nil
}

// StorageTaskID returns the configured storage-task ID. Used by callers that
// need to skip persistence for the storage task itself (recursion guard).
func (s *Store) StorageTaskID() string { return s.storageTask }

// timeNow is a test seam allowing tests to freeze time for deterministic
// stored_at values.
var timeNow = func() time.Time { return time.Now() }
