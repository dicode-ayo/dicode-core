package relay

import (
	"testing"
	"time"
)

// TestNewReconnectBackoff_Defaults locks in the parameters fed to
// cenkalti/backoff so we don't accidentally drift away from the original
// hand-rolled math (1s initial, 60s cap, ±20% jitter, runs forever).
func TestNewReconnectBackoff_Defaults(t *testing.T) {
	bo := newReconnectBackoff()
	if bo.InitialInterval != time.Second {
		t.Errorf("InitialInterval = %v; want 1s", bo.InitialInterval)
	}
	if bo.MaxInterval != 60*time.Second {
		t.Errorf("MaxInterval = %v; want 60s", bo.MaxInterval)
	}
	if bo.RandomizationFactor != 0.2 {
		t.Errorf("RandomizationFactor = %v; want 0.2", bo.RandomizationFactor)
	}
	if bo.Multiplier != 2 {
		t.Errorf("Multiplier = %v; want 2", bo.Multiplier)
	}
	// MaxElapsedTime == 0 is the cenkalti convention for "never stop".
	// The relay reconnect loop is supervised by ctx, not an elapsed-time
	// deadline; if this regresses to non-zero, NextBackOff() will start
	// returning Stop and reconnects will silently break.
	if bo.MaxElapsedTime != 0 {
		t.Errorf("MaxElapsedTime = %v; want 0 (run forever)", bo.MaxElapsedTime)
	}
}

// TestNewReconnectBackoff_FirstCallReturnsInitialInterval pins the contract
// that the very first NextBackOff() returns ~InitialInterval, not zero.
// Old hand-rolled code started at `time.Second` before jitter; a future swap
// to a backoff implementation that started at 0 would make the first
// reconnect attempt fire instantly, hammering a flapping relay.
func TestNewReconnectBackoff_FirstCallReturnsInitialInterval(t *testing.T) {
	bo := newReconnectBackoff()
	d := bo.NextBackOff()
	const lo = time.Duration(float64(time.Second) * 0.8)
	const hi = time.Duration(float64(time.Second) * 1.2)
	if d < lo || d > hi {
		t.Errorf("first NextBackOff() = %v; want in [%v, %v] (1s ±20%%)", d, lo, hi)
	}
}

// TestNewReconnectBackoff_NeverStops drives the backoff well past any
// plausible elapsed-time budget and verifies NextBackOff() never returns
// the sentinel Stop value. Defends against a future MaxElapsedTime tweak
// silently breaking the reconnect loop.
func TestNewReconnectBackoff_NeverStops(t *testing.T) {
	bo := newReconnectBackoff()
	for i := 0; i < 50; i++ {
		if d := bo.NextBackOff(); d < 0 {
			t.Fatalf("NextBackOff() returned Stop on iteration %d", i)
		}
	}
}

// TestNewReconnectBackoff_RespectsMaxInterval checks that the randomized
// interval stays within the [1-r, 1+r]*MaxInterval band once it saturates.
// This guards against a multiplier change accidentally letting the wait
// drift well above 60s.
func TestNewReconnectBackoff_RespectsMaxInterval(t *testing.T) {
	bo := newReconnectBackoff()
	// Drive the backoff until it saturates at MaxInterval (well past it).
	for i := 0; i < 20; i++ {
		bo.NextBackOff()
	}
	// Now sample a few more — they should all be within ±20% of 60s.
	const lo = time.Duration(float64(60*time.Second) * 0.8)
	const hi = time.Duration(float64(60*time.Second) * 1.2)
	for i := 0; i < 10; i++ {
		d := bo.NextBackOff()
		if d < lo || d > hi {
			t.Errorf("NextBackOff() = %v; want in [%v, %v] after saturation", d, lo, hi)
		}
	}
}

// TestNewReconnectBackoff_ResetRewindsToFloor verifies that calling
// Reset() after the backoff has grown returns NextBackOff() to the 1s
// initial-interval band. The relay Run loop relies on this to drop back
// to a 1s reconnect after a stable connection finally drops.
func TestNewReconnectBackoff_ResetRewindsToFloor(t *testing.T) {
	bo := newReconnectBackoff()
	// Saturate.
	for i := 0; i < 20; i++ {
		bo.NextBackOff()
	}
	bo.Reset()
	d := bo.NextBackOff()
	const lo = time.Duration(float64(time.Second) * 0.8)
	const hi = time.Duration(float64(time.Second) * 1.2)
	if d < lo || d > hi {
		t.Errorf("NextBackOff() after Reset = %v; want in [%v, %v] (1s ±20%%)", d, lo, hi)
	}
}
