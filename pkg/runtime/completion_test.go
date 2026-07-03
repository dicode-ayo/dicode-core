package runtime

import (
	"errors"
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
	terminated := false
	exitErr, exitedFirst := AwaitBridgeCompletion(returnCh, doneCh, time.Minute,
		func(v any) {
			got = v
			// Simulate the process exiting (with a nonzero status) right
			// after posting its return value. Filling doneCh here — after
			// the return branch has been taken — keeps the select
			// deterministic.
			doneCh <- errors.New("exit status 1") // ignored: return already arrived
		},
		func() { terminated = true },
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
	if terminated {
		t.Error("terminate fired although the process exited within grace")
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

	terminated := false
	start := time.Now()
	exitErr, exitedFirst := AwaitBridgeCompletion(returnCh, doneCh, 20*time.Millisecond,
		func(any) {},
		func() { terminated = true },
	)

	if !terminated {
		t.Fatal("terminate did not fire after the grace window")
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

	exitErr, exitedFirst := AwaitBridgeCompletion(returnCh, doneCh, time.Minute,
		func(any) { t.Error("onReturn called although no return value was posted") },
		func() { t.Error("terminate fired on the exit-first path") },
	)

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
		exitErr, _ := AwaitBridgeCompletion(returnCh, doneCh, time.Minute,
			func(v any) { got = v },
			func() {},
		)

		if exitErr != nil {
			t.Fatalf("clean exit: exitErr = %v, want nil", exitErr)
		}
		if got != "late-return" {
			t.Fatalf("raced return value not delivered: got %v", got)
		}
	}
}
