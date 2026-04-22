package onboarding

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"sync"
)

// GeneratePIN returns a 6-digit numeric PIN generated via crypto/rand.
// Uniform over [000000, 999999] by rejection sampling past 2^32 mod 10^6.
//
// The PIN is printed to the daemon's stdout on first-run onboarding and
// entered by the user in the browser wizard — it gates /setup/apply
// without embedding a secret in argv or the URL (which would leak via
// /proc/<pid>/cmdline to any local UID).
func GeneratePIN() string {
	const n = 1_000_000
	// 2^32 / 1_000_000 = 4294, remainder 967296. Reject values >= 4294000000
	// so the modulo is unbiased.
	const max = 4294000000
	var b [4]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic(err) // crypto/rand never fails on modern kernels
		}
		v := binary.BigEndian.Uint32(b[:])
		if v >= max {
			continue
		}
		return fmt.Sprintf("%06d", v%n)
	}
}

// pinGate enforces at most maxAttempts PIN checks. Subsequent Check calls
// — even with the correct PIN — return false. Single-session; caller
// instantiates one per wizard run.
type pinGate struct {
	pin         string
	maxAttempts int

	mu       sync.Mutex
	attempts int
}

func newPinGate(pin string, maxAttempts int) *pinGate {
	return &pinGate{pin: pin, maxAttempts: maxAttempts}
}

// Check returns true iff got matches the PIN AND the attempts counter is
// below the lockout threshold. Wrong attempts increment the counter;
// once the counter reaches maxAttempts, all subsequent checks return
// false regardless of got.
//
// Constant-time comparison prevents timing leaks of partial-match
// information about the PIN.
func (g *pinGate) Check(got string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.attempts >= g.maxAttempts {
		return false
	}
	ok := len(got) == len(g.pin) &&
		subtle.ConstantTimeCompare([]byte(got), []byte(g.pin)) == 1
	if !ok {
		g.attempts++
	}
	return ok
}

// Locked reports whether the gate has exhausted its attempt budget.
// Callers use it to distinguish "wrong PIN, try again" from
// "session locked, restart the daemon" in user-facing messages.
func (g *pinGate) Locked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempts >= g.maxAttempts
}
