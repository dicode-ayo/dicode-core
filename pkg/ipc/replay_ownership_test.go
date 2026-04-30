package ipc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ipcFakeReplayRunner satisfies registry.ReplayRunner for IPC ownership tests.
// It always returns a new run ID and records calls.
type ipcFakeReplayRunner struct{}

func (ipcFakeReplayRunner) FireForReplay(_ context.Context, _, _ string, _ any) (string, error) {
	return uuid.New().String(), nil
}

// ipcFakeTaskRunner satisfies registry.TaskRunner for the InputStore.
type ipcFakeTaskRunner struct{}

func (ipcFakeTaskRunner) RunTaskSync(_ context.Context, _ string, _ map[string]string) (any, error) {
	return map[string]any{"ok": true}, nil
}

// newTestReplayer returns a registry.Replayer backed by the given registry.
func newTestReplayer(reg *registry.Registry) *registry.Replayer {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	is := registry.NewInputStore(registry.NewInputCrypto(key), ipcFakeTaskRunner{}, "fake-storage")
	return registry.NewReplayer(reg, is, ipcFakeReplayRunner{})
}

// insertRunWithTask inserts a run row owned by taskID with a non-empty
// InputStorageKey so the replay ownership check is reached.
func insertRunWithTask(t *testing.T, reg *registry.Registry, d interface {
	Exec(ctx context.Context, query string, args ...any) error
}, runID, taskID string) {
	t.Helper()
	err := d.Exec(context.Background(),
		`INSERT INTO runs(id, task_id, status, started_at, parent_run_id, trigger_source, input_storage_key, input_stored_at)
		 VALUES (?,?,?,?,NULL,?,?,?)`,
		runID, taskID, "success", 0, "manual", "key1", 0)
	if err != nil {
		t.Fatalf("insertRunWithTask: %v", err)
	}
}

// TestIPC_Replay_RejectsCrossTaskWithoutLineage verifies that a server whose
// taskID is "task-b" cannot replay a run owned by "task-a" when there is no
// parent-run lineage. The IPC handler must return an error containing
// "not permitted".
func TestIPC_Replay_RejectsCrossTaskWithoutLineage(t *testing.T) {
	e := newTestEnv(t)

	// Insert a run owned by task-a with a non-empty input storage key.
	targetRunID := fmt.Sprintf("target-run-%d", time.Now().UnixNano())
	insertRunWithTask(t, e.reg, e.db, targetRunID, "task-a")

	// Build a spec with RunsReplay granted.
	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				RunsReplay: true,
			},
		},
	}

	// Create the server with taskID = "task-b" directly (startWithSpec always
	// uses "test-task"). The server's own runID is not in the registry so
	// GetRun will fail → callerParentRunID stays "".
	callerRunID := fmt.Sprintf("caller-run-%d", time.Now().UnixNano())
	srv := New(callerRunID, "task-b", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	srv.SetReplayer(newTestReplayer(e.reg))

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	// Attempt to replay a run owned by task-a from task-b with no lineage.
	sendMsg(t, conn, map[string]any{
		"id":     "replay-cross",
		"method": "dicode.runs.replay",
		"runID":  targetRunID,
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	// ErrReplayNotPermitted message: "replay: caller task may not replay this run"
	if !strings.Contains(errMsg, "may not replay") {
		t.Errorf("expected 'may not replay' error, got %q (full resp: %#v)", errMsg, resp)
	}
}

// TestIPC_Replay_AllowsSelfTask verifies that a server whose taskID is
// "task-a" can replay a run owned by "task-a" (same task, self-replay).
func TestIPC_Replay_AllowsSelfTask(t *testing.T) {
	e := newTestEnv(t)

	targetRunID := fmt.Sprintf("target-run-self-%d", time.Now().UnixNano())
	insertRunWithTask(t, e.reg, e.db, targetRunID, "task-a")

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				RunsReplay: true,
			},
		},
	}

	callerRunID := fmt.Sprintf("caller-run-self-%d", time.Now().UnixNano())
	srv := New(callerRunID, "task-a", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	srv.SetReplayer(newTestReplayer(e.reg))

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{
		"id":     "replay-self",
		"method": "dicode.runs.replay",
		"runID":  targetRunID,
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	// The ownership check passes (same task), but the stored input is not
	// actually accessible via the fake store, so we expect a fetch/storage
	// error — NOT the ownership sentinel. This confirms the ownership check
	// did not reject the call.
	if strings.Contains(errMsg, "may not replay") {
		t.Errorf("self-task replay was incorrectly rejected by ownership check: %q", errMsg)
	}
}
