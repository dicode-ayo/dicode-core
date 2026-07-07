package ipc

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// TestControlServer_ReadinessGating verifies the readiness barrier is consulted
// exactly for task-scoped control methods and skipped for the others (issue
// #464): cli.run, cli.task.test, and cli.status-with-id wait; cli.list and
// cli.status-without-id do not.
func TestControlServer_ReadinessGating(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	var waits int32
	cs := &ControlServer{
		reg:    reg,
		engine: &mockEngine{runID: "r1", result: RunResult{RunID: "r1", Status: "success"}},
		log:    zap.NewNop(),
	}
	cs.SetReadinessWaiter(func(ctx context.Context) bool {
		atomic.AddInt32(&waits, 1)
		return true
	})

	cases := []struct {
		name     string
		req      Request
		wantWait bool
	}{
		{"run waits", Request{Method: "cli.run", TaskID: "buildin/webui"}, true},
		{"task.test waits", Request{Method: "cli.task.test", TaskID: "buildin/webui"}, true},
		{"status with id waits", Request{Method: "cli.status", TaskID: "buildin/webui"}, true},
		{"status without id does not wait", Request{Method: "cli.status"}, false},
		{"list does not wait", Request{Method: "cli.list"}, false},
		{"ping does not wait", Request{Method: "cli.ping"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			atomic.StoreInt32(&waits, 0)
			// Errors from the handlers (e.g. "no runs found") are irrelevant here;
			// we only assert whether the readiness barrier was consulted.
			_, _ = cs.dispatch(context.Background(), tc.req)
			got := atomic.LoadInt32(&waits) > 0
			if got != tc.wantWait {
				t.Fatalf("%s: readiness consulted = %v, want %v", tc.req.Method, got, tc.wantWait)
			}
		})
	}
}

// TestControlServer_ReadinessBlocksUntilRegistered proves the wait happens
// before the lookup: the waiter registers the task, and only then does the
// task-scoped handler find it. Without the barrier the run would dispatch
// against an empty registry.
func TestControlServer_ReadinessBlocksUntilRegistered(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	eng := &mockEngine{runID: "r1", result: RunResult{RunID: "r1", Status: "success"}}
	cs := &ControlServer{reg: reg, engine: eng, log: zap.NewNop()}

	var readyChecked bool
	cs.SetReadinessWaiter(func(ctx context.Context) bool {
		readyChecked = true
		return true
	})

	res, err := cs.handleRun(context.Background(), Request{TaskID: "buildin/webui"})
	if err != nil {
		t.Fatalf("handleRun: %v", err)
	}
	if !readyChecked {
		t.Fatal("handleRun dispatched without consulting the readiness barrier")
	}
	if res.RunID != "r1" {
		t.Fatalf("unexpected run result: %+v", res)
	}
}
