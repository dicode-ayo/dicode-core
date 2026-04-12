package metrics

import "testing"

func TestReadDaemonMetrics(t *testing.T) {
	m := ReadDaemonMetrics()
	if m.HeapAllocMB <= 0 {
		t.Errorf("expected HeapAllocMB > 0, got %f", m.HeapAllocMB)
	}
	if m.HeapSysMB <= 0 {
		t.Errorf("expected HeapSysMB > 0, got %f", m.HeapSysMB)
	}
	if m.Goroutines <= 0 {
		t.Errorf("expected Goroutines > 0, got %d", m.Goroutines)
	}
}

func TestReadChildMetrics_NoPIDs(t *testing.T) {
	c := ReadChildMetrics(nil, 3)
	if c.ActiveTasks != 3 {
		t.Errorf("expected ActiveTasks=3, got %d", c.ActiveTasks)
	}
	if c.ChildRSSMB != 0 {
		t.Errorf("expected ChildRSSMB=0 with no pids, got %f", c.ChildRSSMB)
	}
}
