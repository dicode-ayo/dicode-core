package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// WaitRun and WaitRunSettled deliberately disagree about a suspended run:
// dicode.run_task wants "block until genuinely terminal" (#516), while the CLI
// must observe `suspended` to render the resume form. Collapsing the two hangs
// `dicode run` on any task that suspends.
func waitEnv(t *testing.T) (*Engine, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	return New(reg, immediateExec{}, zap.NewNop()), reg
}

func suspendedRun(t *testing.T, reg *registry.Registry) string {
	t.Helper()
	ctx := context.Background()
	runID, err := reg.StartRun(ctx, "wizard", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	schema := []byte(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	ok, err := reg.SuspendRun(ctx, runID, []byte(`{}`), schema, "tok", 1, 0, nil, nil)
	if err != nil || !ok {
		t.Fatalf("SuspendRun: ok=%v err=%v", ok, err)
	}
	return runID
}

func TestWaitRunSettled_ReturnsSuspendedImmediately(t *testing.T) {
	eng, reg := waitEnv(t)
	runID := suspendedRun(t, reg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	var got string
	go func() {
		res, err := eng.WaitRunSettled(ctx, runID)
		if err == nil {
			got = res.Status
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitRunSettled blocked on a suspended run; the CLI would never render the resume form")
	}
	if got != string(registry.StatusSuspended) {
		t.Fatalf("status = %q, want %q", got, registry.StatusSuspended)
	}
}

// The complement: WaitRun must NOT settle on `suspended` — it follows the resume
// chain. A caller that wants the pause has to ask for it.
func TestWaitRun_DoesNotReturnWhileSuspended(t *testing.T) {
	eng, reg := waitEnv(t)
	runID := suspendedRun(t, reg)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		eng.WaitRun(ctx, runID) //nolint:errcheck
		close(done)
	}()

	select {
	case <-done:
		// Returned only because ctx expired, which is the contract: bounded by the
		// caller's timeout, not by the run reaching `suspended`.
		if ctx.Err() == nil {
			t.Fatal("WaitRun returned on a suspended run; it must follow the resume chain")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitRun ignored its context deadline")
	}
}
