package trigger

import (
	"testing"
	"time"
)

func TestReplayCache_RejectsSecondUse(t *testing.T) {
	c := newReplayCache(1 * time.Hour)
	if c.seen("abc123") {
		t.Fatal("first use should not be seen")
	}
	if !c.seen("abc123") {
		t.Fatal("second use should be seen")
	}
}

func TestReplayCache_DifferentDigestsAllowed(t *testing.T) {
	c := newReplayCache(1 * time.Hour)
	if c.seen("abc") {
		t.Fatal("first digest should not be seen")
	}
	if c.seen("def") {
		t.Fatal("different digest should not be seen")
	}
}

func TestReplayCache_ExpiredEntryAllowed(t *testing.T) {
	c := newReplayCache(50 * time.Millisecond)
	if c.seen("abc") {
		t.Fatal("first use should not be seen")
	}
	time.Sleep(100 * time.Millisecond)
	if c.seen("abc") {
		t.Fatal("expired entry should be allowed")
	}
}

func TestReplayCache_EvictsOldestAtCapacity(t *testing.T) {
	c := newReplayCache(1 * time.Hour)
	c.maxEntries = 3
	c.seen("a")
	c.seen("b")
	c.seen("c")
	c.seen("d")
	if c.seen("a") {
		t.Fatal("evicted entry should be allowed again")
	}
}
