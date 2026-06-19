package ipc

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// TestIPCSocket_Mode0600 verifies that the per-task Unix socket is created
// with mode 0600 so that other local users on a multi-user host cannot
// connect to it (fix for #383 MEDIUM finding).
func TestIPCSocket_Mode0600(t *testing.T) {
	e := newTestEnv(t)
	runID := "sec383-sock-" + time.Now().Format("20060102150405")
	srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)

	socketPath, _, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	fi, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	mode := fi.Mode().Perm()
	if mode != 0600 {
		t.Errorf("socket mode: got %04o, want 0600", mode)
	}
}

// TestCryptoContextAllowed_RejectsDicodePrefix verifies that the "dicode/"
// namespace is always denied to tasks, even when crypto:["*"] is granted
// (fix for #383 MEDIUM finding). A task with a "*" grant could otherwise
// derive the same sub-key the daemon uses to encrypt persisted run inputs.
func TestCryptoContextAllowed_RejectsDicodePrefix(t *testing.T) {
	cases := []struct {
		name    string
		granted []string
		ctx     string
		allowed bool
	}{
		// Daemon-private context — always denied.
		{"run-inputs denied with wildcard", []string{"*"}, "dicode/run-inputs/v1", false},
		{"run-inputs denied with exact grant", []string{"dicode/run-inputs/v1"}, "dicode/run-inputs/v1", false},
		// Buildin-task contexts — allowed when granted (not daemon-private).
		{"relay-identity allowed with wildcard", []string{"*"}, "dicode/relay-identity/v1", true},
		{"relay-identity allowed with explicit", []string{"dicode/relay-identity/v1"}, "dicode/relay-identity/v1", true},
		{"relay-identity denied without grant", []string{"my-ctx"}, "dicode/relay-identity/v1", false},
		// Regular user contexts.
		{"user ctx with wildcard", []string{"*"}, "my-app/secrets", true},
		{"user ctx explicit", []string{"my-app/secrets"}, "my-app/secrets", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &task.Spec{
				Permissions: task.Permissions{
					Dicode: &task.DicodePermissions{
						Crypto: tc.granted,
					},
				},
			}
			e := newTestEnv(t)
			runID := "sec383-crypto-" + time.Now().Format("20060102150405.000000")
			srv := New(runID, "test-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
			got := srv.cryptoContextAllowed(tc.ctx)
			if got != tc.allowed {
				t.Errorf("cryptoContextAllowed(%q) with grants %v = %v, want %v",
					tc.ctx, tc.granted, got, tc.allowed)
			}
		})
	}
}

// TestIPC_Crypto_RejectsDaemonNamespace exercises the full IPC path: a task
// with crypto:["*"] sends a dicode.crypto.encrypt request for a "dicode/"
// context and must receive a permission-denied error, not a success.
func TestIPC_Crypto_RejectsDaemonNamespace(t *testing.T) {
	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				Crypto: []string{"*"},
			},
		},
	}
	e := newTestEnv(t)
	conn, srv := e.startWithSpec(t, nil, nil, spec, nil)
	_ = srv

	// Wire a fake sub-key deriver so the handler doesn't panic.
	srv.SetCryptoHandler(&fakeDeriver{})

	sendMsg(t, conn, map[string]any{
		"id":            "crypto-deny",
		"method":        "dicode.crypto.encrypt",
		"context":       "dicode/run-inputs/v1",
		"plaintext_b64": "aGVsbG8=", // base64("hello")
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "not in permissions") && !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected permission denied error for dicode/ context, got %q", errMsg)
	}
}

// TestIPC_Replay_RejectsTaskNameRetarget verifies that a task-scoped caller
// that passes the ownership check (replaying its own run) still cannot
// redirect the replay at a different task via the taskName override
// (fix for #383 MEDIUM finding).
func TestIPC_Replay_RejectsTaskNameRetarget(t *testing.T) {
	e := newTestEnv(t)

	targetRunID := "sec383-replay-" + time.Now().Format("20060102150405.000000")
	// Run is owned by "task-a".
	insertRunWithTask(t, e.reg, e.db, targetRunID, "task-a")

	spec := &task.Spec{
		Permissions: task.Permissions{
			Dicode: &task.DicodePermissions{
				RunsReplay: true,
			},
		},
	}

	// Caller is task-a (ownership passes), but tries to replay into "task-b".
	callerRunID := "sec383-caller-" + time.Now().Format("20060102150405.000000")
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
		"id":     "replay-retarget",
		"method": "dicode.runs.replay",
		"runID":  targetRunID,
		"taskID": "task-b", // attempt to retarget (req.TaskID is the taskName override)
	})
	resp := recvMsg(t, conn)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "may not replay") {
		t.Errorf("expected 'may not replay' error for cross-task retarget, got %q (resp: %#v)", errMsg, resp)
	}
}

// fakeDeriver satisfies SubKeyDeriver for tests that don't reach the actual
// encrypt/decrypt path (they are rejected before the deriver is called).
type fakeDeriver struct{}

func (fakeDeriver) DeriveSubKey(_ string) ([]byte, error) {
	key := make([]byte, 32)
	return key, nil
}
