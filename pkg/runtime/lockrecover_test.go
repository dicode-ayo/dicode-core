package runtime

import (
	"errors"
	"strings"
	"testing"
)

// TestRecoverStaleLock_SucceedsAfterOneRelock: a stale-lock failure triggers a
// single relock and a single retry that then succeeds.
func TestRecoverStaleLock_SucceedsAfterOneRelock(t *testing.T) {
	var attempts, relocks int
	attempt := func() bool {
		attempts++
		return attempts == 1 // first run stale, retry succeeds
	}
	relock := func() error { relocks++; return nil }

	retried, err := RecoverStaleLock(true, attempt, relock)
	if err != nil {
		t.Fatalf("unexpected relock error: %v", err)
	}
	if !retried {
		t.Error("expected retried=true")
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if relocks != 1 {
		t.Errorf("relocks = %d, want 1", relocks)
	}
}

// TestRecoverStaleLock_NonLockFailureNotRetried: a failure that is not the
// stale-lock signature must not relock or retry.
func TestRecoverStaleLock_NonLockFailureNotRetried(t *testing.T) {
	var attempts, relocks int
	attempt := func() bool { attempts++; return false } // not a stale-lock failure
	relock := func() error { relocks++; return nil }

	retried, err := RecoverStaleLock(true, attempt, relock)
	if err != nil || retried {
		t.Fatalf("retried=%v err=%v, want false/nil", retried, err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if relocks != 0 {
		t.Errorf("relocks = %d, want 0", relocks)
	}
}

// TestRecoverStaleLock_BoundedToOne: even when every attempt reports stale, the
// relock+retry happens exactly once — no thrashing on a genuinely broken lock.
func TestRecoverStaleLock_BoundedToOne(t *testing.T) {
	var attempts, relocks int
	attempt := func() bool { attempts++; return true } // always stale
	relock := func() error { relocks++; return nil }

	retried, err := RecoverStaleLock(true, attempt, relock)
	if err != nil {
		t.Fatalf("unexpected relock error: %v", err)
	}
	if !retried {
		t.Error("expected retried=true")
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (bounded to one retry)", attempts)
	}
	if relocks != 1 {
		t.Errorf("relocks = %d, want 1", relocks)
	}
}

// TestRecoverStaleLock_Disabled: with recovery disabled, a stale-lock failure
// is left as-is — attempt runs once, no relock.
func TestRecoverStaleLock_Disabled(t *testing.T) {
	var attempts, relocks int
	attempt := func() bool { attempts++; return true }
	relock := func() error { relocks++; return nil }

	retried, _ := RecoverStaleLock(false, attempt, relock)
	if retried {
		t.Error("expected retried=false when disabled")
	}
	if attempts != 1 || relocks != 0 {
		t.Errorf("attempts=%d relocks=%d, want 1/0", attempts, relocks)
	}
}

// TestRecoverStaleLock_RelockErrorSurfacesOriginal: when relock itself fails,
// there is no retry and the relock error is returned so the caller can leave
// the original run failure in place.
func TestRecoverStaleLock_RelockErrorSurfacesOriginal(t *testing.T) {
	var attempts int
	relockErr := errors.New("relock boom")
	attempt := func() bool { attempts++; return true }
	relock := func() error { return relockErr }

	retried, err := RecoverStaleLock(true, attempt, relock)
	if retried {
		t.Error("expected retried=false when relock fails")
	}
	if !errors.Is(err, relockErr) {
		t.Errorf("err = %v, want relock error", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry after relock failure)", attempts)
	}
}

func TestLockErrSniffer_MatchAndSplit(t *testing.T) {
	t.Run("single write", func(t *testing.T) {
		s := NewLockErrSniffer("lockfile is out of date")
		n, err := s.Write([]byte("error: The lockfile is out of date. Rerun ...\n"))
		if err != nil || n == 0 {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if !s.StaleLock() {
			t.Error("expected match")
		}
	})

	t.Run("split across writes", func(t *testing.T) {
		s := NewLockErrSniffer("`--locked` was provided")
		s.Write([]byte("error: The lockfile needs to be updated, but `--loc")) //nolint:errcheck
		if s.StaleLock() {
			t.Fatal("matched too early")
		}
		s.Write([]byte("ked` was provided. To update ...")) //nolint:errcheck
		if !s.StaleLock() {
			t.Error("expected match across split writes")
		}
	})

	t.Run("no match on ordinary error", func(t *testing.T) {
		s := NewLockErrSniffer("lockfile is out of date")
		s.Write([]byte("TypeError: undefined is not a function\n    at main\n")) //nolint:errcheck
		if s.StaleLock() {
			t.Error("ordinary error must not match")
		}
	})
}

// TestLockErrSniffer_WriteContract: Write must always report the full length as
// consumed (it is teed off the log pipe and must never short-write).
func TestLockErrSniffer_WriteContract(t *testing.T) {
	s := NewLockErrSniffer("x")
	big := strings.Repeat("y", staleLockSniffCap*2)
	n, err := s.Write([]byte(big))
	if err != nil {
		t.Fatalf("Write err: %v", err)
	}
	if n != len(big) {
		t.Errorf("n = %d, want %d", n, len(big))
	}
}

// TestStaleLockRecoveryEnabled_OptOut: recovery is on by default and the env
// opt-out turns it off.
func TestStaleLockRecoveryEnabled_OptOut(t *testing.T) {
	if !StaleLockRecoveryEnabled() {
		t.Error("recovery should be enabled by default")
	}
	t.Setenv("DICODE_DISABLE_LOCK_AUTORECOVERY", "1")
	if StaleLockRecoveryEnabled() {
		t.Error("DICODE_DISABLE_LOCK_AUTORECOVERY=1 should disable recovery")
	}
}

func TestShortHash(t *testing.T) {
	if got := ShortHash(nil); got != "absent" {
		t.Errorf("nil hash = %q, want absent", got)
	}
	a := ShortHash([]byte("one"))
	b := ShortHash([]byte("two"))
	if a == "" || a == "absent" {
		t.Errorf("unexpected hash %q", a)
	}
	if a == b {
		t.Error("distinct inputs hashed equal")
	}
	if a != ShortHash([]byte("one")) {
		t.Error("hash not deterministic")
	}
}
