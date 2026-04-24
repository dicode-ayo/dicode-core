package taskset

import (
	"errors"
	"testing"
	"time"
)

func TestSource_PullStatus_InitialZero(t *testing.T) {
	s := &Source{}
	ps := s.PullStatus()
	if !ps.LastPullAt.IsZero() {
		t.Errorf("LastPullAt should be zero on a fresh Source; got %v", ps.LastPullAt)
	}
	if ps.OK {
		t.Error("OK should be false on a fresh Source")
	}
	if ps.Error != "" {
		t.Errorf("Error should be empty; got %q", ps.Error)
	}
}

func TestSource_RecordPull_Success(t *testing.T) {
	s := &Source{}
	before := time.Now()
	s.recordPull(nil)
	ps := s.PullStatus()
	if !ps.OK {
		t.Error("OK should be true after recordPull(nil)")
	}
	if ps.Error != "" {
		t.Errorf("Error should be empty on success; got %q", ps.Error)
	}
	if ps.LastPullAt.Before(before) {
		t.Errorf("LastPullAt = %v; should be >= %v", ps.LastPullAt, before)
	}
}

func TestSource_RecordPull_Failure(t *testing.T) {
	s := &Source{}
	s.recordPull(errors.New("pull: object not found"))
	ps := s.PullStatus()
	if ps.OK {
		t.Error("OK should be false after recordPull(err)")
	}
	if ps.Error != "pull: object not found" {
		t.Errorf("Error = %q; want pull: object not found", ps.Error)
	}
	if ps.LastPullAt.IsZero() {
		t.Error("LastPullAt should be set even on failure")
	}
}

func TestSource_RecordPull_ErrorClearedOnNextSuccess(t *testing.T) {
	s := &Source{}
	s.recordPull(errors.New("boom"))
	s.recordPull(nil)
	ps := s.PullStatus()
	if !ps.OK {
		t.Error("OK should be true after success follows failure")
	}
	if ps.Error != "" {
		t.Errorf("Error should be cleared after subsequent success; got %q", ps.Error)
	}
}
