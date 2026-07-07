package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// StaleLockRecoveryEnabled reports whether a runtime should deterministically
// regenerate a stale lockfile and retry the run once. Recovery is on by
// default: regenerating a lock for an already-approved task re-pins the
// dependencies the task already declares, so the approval gate (which governs
// task *content*, not lock resolution) is not bypassed. Set
// DICODE_DISABLE_LOCK_AUTORECOVERY=1 to keep a stale lock a hard failure — the
// escape hatch for deployments that treat any lock drift as a supply-chain
// signal to investigate rather than heal.
func StaleLockRecoveryEnabled() bool {
	return os.Getenv("DICODE_DISABLE_LOCK_AUTORECOVERY") != "1"
}

// staleLockSniffCap bounds how much subprocess stderr a LockErrSniffer retains
// while looking for the stale-lock signature. The runtime emits the lockfile
// diagnostic as its first output, so a small window catches it without
// buffering an unbounded error stream.
const staleLockSniffCap = 64 << 10

// LockErrSniffer is an io.Writer that watches a subprocess's output for a
// runtime's stale-lock signature (Deno's "lockfile is out of date", uv's
// "--locked was provided"). It is teed off the log-streaming pipe so the same
// bytes still reach the run log; it only records whether the signature
// appeared, capping retained bytes so a chatty task cannot grow it without
// bound. A signature may straddle two writes, so it re-scans the retained
// buffer on each write until matched.
type LockErrSniffer struct {
	sigs [][]byte
	buf  []byte
	hit  bool
}

// NewLockErrSniffer returns a sniffer that matches any of sigs.
func NewLockErrSniffer(sigs ...string) *LockErrSniffer {
	s := &LockErrSniffer{}
	for _, sig := range sigs {
		s.sigs = append(s.sigs, []byte(sig))
	}
	return s
}

func (s *LockErrSniffer) Write(p []byte) (int, error) {
	if !s.hit && len(s.buf) < staleLockSniffCap {
		if room := staleLockSniffCap - len(s.buf); len(p) > room {
			s.buf = append(s.buf, p[:room]...)
		} else {
			s.buf = append(s.buf, p...)
		}
		for _, sig := range s.sigs {
			if bytes.Contains(s.buf, sig) {
				s.hit = true
				s.buf = nil // matched; stop retaining
				break
			}
		}
	}
	return len(p), nil
}

// StaleLock reports whether a stale-lock signature was seen.
func (s *LockErrSniffer) StaleLock() bool { return s.hit }

// RecoverStaleLock runs a subprocess attempt and, when it fails with a
// stale-lock signature and recovery is enabled, regenerates the lock once and
// retries the attempt exactly one more time. attempt performs the run and
// reports whether it failed with the stale-lock signature; relock regenerates
// the lock. Recovery is bounded to a single relock+retry — a second stale
// result is left as the run's outcome, so a genuinely broken lock cannot
// thrash. Returns whether a relock+retry happened and any error from relock
// (in which case the original failure stands and no retry is attempted).
func RecoverStaleLock(enabled bool, attempt func() (staleLock bool), relock func() error) (retried bool, relockErr error) {
	stale := attempt()
	if !stale || !enabled {
		return false, nil
	}
	if err := relock(); err != nil {
		return false, err
	}
	attempt()
	return true, nil
}

// ShortHash returns a short hex digest of b for audit lines, or "absent" when
// b is nil (the lockfile did not exist). Used to record the before/after lock
// content delta when a stale lock is auto-regenerated.
func ShortHash(b []byte) string {
	if b == nil {
		return "absent"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}
