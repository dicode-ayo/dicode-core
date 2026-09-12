package runtime

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// The AwaitBridgeCompletion tests pin the completion protocol both
// socket-bridge runtimes implemented by hand before issue #388: return-first
// vs exit-first arbitration, the grace-then-terminate nudge, and the
// exit-status-ignored-after-return rule.

// TestAwaitBridgeCompletion_ReturnThenExit is the happy path: the task posts
// its return value and the process exits within the grace window. terminate
// must NOT fire and the exit status must be ignored.
func TestAwaitBridgeCompletion_ReturnThenExit(t *testing.T) {
	returnCh := make(chan any, 1)
	doneCh := make(chan error, 1)
	returnCh <- "the-result"

	var got any
	proc := &stubProc{}
	exitErr, exitedFirst := AwaitBridgeCompletion(returnCh, doneCh, time.Minute, time.Minute,
		func(v any) {
			got = v
			// Simulate the process exiting (with a nonzero status) right
			// after posting its return value. Filling doneCh here — after
			// the return branch has been taken — keeps the select
			// deterministic.
			doneCh <- errors.New("exit status 1") // ignored: return already arrived
		},
		proc,
	)

	if got != "the-result" {
		t.Errorf("onReturn got %v, want the-result", got)
	}
	if exitedFirst {
		t.Error("exitedFirst = true, want false (return arrived first)")
	}
	if exitErr != nil {
		t.Errorf("exit status after a return must be ignored, got %v", exitErr)
	}
	if terminated, _ := proc.state(); terminated {
		t.Error("SIGTERM sent although the process exited within grace")
	}
}

// TestAwaitBridgeCompletion_ReturnThenHang verifies the nudge: when the
// process does not exit within grace after posting its return value,
// terminate fires (the runtimes send SIGTERM) and the helper returns without
// waiting for the process.
func TestAwaitBridgeCompletion_ReturnThenHang(t *testing.T) {
	returnCh := make(chan any, 1)
	doneCh := make(chan error, 1) // never receives: process hangs
	returnCh <- 42

	proc := &stubProc{}
	start := time.Now()
	exitErr, exitedFirst := AwaitBridgeCompletion(returnCh, doneCh, 20*time.Millisecond, time.Millisecond,
		func(any) {},
		proc,
	)

	if terminated, _ := proc.state(); !terminated {
		t.Fatal("SIGTERM did not fire after the grace window")
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("terminate fired before the grace window elapsed (%v)", elapsed)
	}
	if exitedFirst || exitErr != nil {
		t.Errorf("got (exitErr=%v, exitedFirst=%v), want (nil, false)", exitErr, exitedFirst)
	}
}

// TestAwaitBridgeCompletion_ExitFirst_Error covers a process that dies
// without posting a return value: the exit error must surface, onReturn must
// not be called, terminate must not fire.
func TestAwaitBridgeCompletion_ExitFirst_Error(t *testing.T) {
	returnCh := make(chan any, 1)
	doneCh := make(chan error, 1)
	wantErr := errors.New("exit status 2")
	doneCh <- wantErr

	proc := &stubProc{}
	exitErr, exitedFirst := AwaitBridgeCompletion(returnCh, doneCh, time.Minute, time.Minute,
		func(any) { t.Error("onReturn called although no return value was posted") },
		proc,
	)
	if terminated, killed := proc.state(); terminated || killed {
		t.Error("the exit-first path signaled a process that had already exited")
	}

	if !exitedFirst {
		t.Error("exitedFirst = false, want true")
	}
	if !errors.Is(exitErr, wantErr) {
		t.Errorf("exitErr = %v, want %v", exitErr, wantErr)
	}
}

// TestAwaitBridgeCompletion_ExitFirst_DrainsRacedReturn covers the race the
// original select handled explicitly: the process exits, but a return value
// arrived just before. The value must be drained (non-blocking) into
// onReturn while the exit error is still reported.
func TestAwaitBridgeCompletion_ExitFirst_DrainsRacedReturn(t *testing.T) {
	// Both channels are readable when the helper runs, mirroring the real
	// race. Whichever branch wins the select, the contract is the same: the
	// return value must reach onReturn and no error may surface (clean
	// exit / exit ignored after return). Iterate to exercise both branches.
	for i := 0; i < 100; i++ {
		returnCh := make(chan any, 1)
		doneCh := make(chan error, 1)
		doneCh <- nil // clean exit
		returnCh <- "late-return"

		got := any(nil)
		exitErr, _ := AwaitBridgeCompletion(returnCh, doneCh, time.Minute, time.Minute,
			func(v any) { got = v },
			&stubProc{},
		)

		if exitErr != nil {
			t.Fatalf("clean exit: exitErr = %v, want nil", exitErr)
		}
		if got != "late-return" {
			t.Fatalf("raced return value not delivered: got %v", got)
		}
	}
}

// stubProc records the signals the completion protocol sends.
type stubProc struct {
	mu         sync.Mutex
	terminated bool
	killed     bool
}

func (p *stubProc) Signal(os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminated = true
	return nil
}

func (p *stubProc) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killed = true
	return nil
}

func (p *stubProc) state() (terminated, killed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminated, p.killed
}

// TestAwaitBridgeCompletion_HangIgnoresSIGTERM: a child that ignores SIGTERM
// holds the run's stderr drain open, so the second grace and the SIGKILL that
// follows it are the only bound on how long the run takes to finish.
func TestAwaitBridgeCompletion_HangIgnoresSIGTERM(t *testing.T) {
	returnCh := make(chan any, 1)
	doneCh := make(chan error, 1) // never receives: the child ignores both
	returnCh <- 42

	proc := &stubProc{}
	start := time.Now()
	AwaitBridgeCompletion(returnCh, doneCh, 20*time.Millisecond, 20*time.Millisecond,
		func(any) {}, proc)

	terminated, killed := proc.state()
	if !terminated {
		t.Error("SIGTERM not sent after the first grace window")
	}
	if !killed {
		t.Error("SIGKILL not sent after the second grace window")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("escalated after %v; want both grace windows to elapse", elapsed)
	}
}

// TestAwaitBridgeCompletion_HangHonorsSIGTERM: a child that exits on SIGTERM
// must not then be killed — the graceful stop is the whole point of sending
// SIGTERM first.
func TestAwaitBridgeCompletion_HangHonorsSIGTERM(t *testing.T) {
	returnCh := make(chan any, 1)
	doneCh := make(chan error, 1)
	returnCh <- 42

	proc := &stubProc{}
	go func() {
		for {
			if terminated, _ := proc.state(); terminated {
				doneCh <- nil
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	AwaitBridgeCompletion(returnCh, doneCh, 20*time.Millisecond, time.Minute,
		func(any) {}, proc)

	terminated, killed := proc.state()
	if !terminated {
		t.Error("SIGTERM not sent after the first grace window")
	}
	if killed {
		t.Error("SIGKILL sent although the child exited on SIGTERM")
	}
}
