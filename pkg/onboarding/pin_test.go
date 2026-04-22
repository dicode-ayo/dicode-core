package onboarding

import (
	"regexp"
	"testing"
)

var digitsOnly = regexp.MustCompile(`^[0-9]{6}$`)

func TestGeneratePIN_Shape(t *testing.T) {
	got := GeneratePIN()
	if !digitsOnly.MatchString(got) {
		t.Errorf("PIN %q is not exactly 6 digits", got)
	}
}

func TestGeneratePIN_Diverse(t *testing.T) {
	// 1000 calls should produce many distinct values. A buggy generator
	// (e.g. one that returns a constant, or heavily biased) would fail.
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		seen[GeneratePIN()] = struct{}{}
	}
	// With 10^6 space and 1000 samples, expected collisions are rare; demand
	// at least 990 unique.
	if len(seen) < 990 {
		t.Errorf("only %d unique PINs in 1000 calls; generator may be biased", len(seen))
	}
}

func TestPinGate_Correct_Accepts(t *testing.T) {
	g := newPinGate("123456", 5)
	if !g.Check("123456") {
		t.Error("correct PIN should be accepted")
	}
}

func TestPinGate_Wrong_Rejects(t *testing.T) {
	g := newPinGate("123456", 5)
	if g.Check("000000") {
		t.Error("wrong PIN should be rejected")
	}
}

func TestPinGate_LockoutAfterMaxAttempts(t *testing.T) {
	g := newPinGate("123456", 3)
	// Burn 3 wrong attempts.
	for i := 0; i < 3; i++ {
		if g.Check("000000") {
			t.Fatalf("wrong PIN accepted at attempt %d", i)
		}
	}
	// Fourth attempt, even with the CORRECT pin, must be rejected.
	if g.Check("123456") {
		t.Error("correct PIN should be rejected after lockout")
	}
}

func TestPinGate_CorrectResetsNothing(t *testing.T) {
	// The gate is single-session — once the wizard submits, the daemon
	// proceeds. We only need the attempts counter; correct PINs don't
	// need to "reset" counters.
	g := newPinGate("123456", 5)
	g.Check("wrong1")
	g.Check("wrong2")
	if !g.Check("123456") {
		t.Error("correct PIN should be accepted while under lockout limit")
	}
}

func TestPinGate_Locked_DistinguishesFromWrong(t *testing.T) {
	g := newPinGate("123456", 2)
	if g.Locked() {
		t.Error("fresh gate should not be locked")
	}
	g.Check("000000")
	g.Check("000001")
	if !g.Locked() {
		t.Error("gate should be locked after attempts exhausted")
	}
}
