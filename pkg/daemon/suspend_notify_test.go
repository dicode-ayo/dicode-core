package daemon

import (
	"testing"

	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// The fire runs on a goroutine (dispatch), so positive cases synchronize on a
// channel; negative cases return before dispatch, so no goroutine is spawned.
func TestSuspendNotifier_FiresOnSuspendWithRenderedAndStructuredParams(t *testing.T) {
	done := make(chan map[string]string, 1)
	n := &suspendNotifier{
		notifyTask: "buildin/notifications",
		resumeURL:  func(runID string) string { return "http://ui/?run=" + runID },
		fire: func(_ string, params map[string]string) error {
			done <- params
			return nil
		},
		log: zap.NewNop(),
	}

	n.onRunFinished("repo/agent", "run-9", string(registry.StatusSuspended), "manual", 0)

	params := <-done
	if params["title"] == "" || params["body"] == "" {
		t.Errorf("buildin/notifications needs title+body; got %+v", params)
	}
	if params["event"] != "suspended" || params["run_id"] != "run-9" || params["task_id"] != "repo/agent" {
		t.Errorf("structured params wrong: %+v", params)
	}
	if params["resume_url"] != "http://ui/?run=run-9" {
		t.Errorf("resume_url = %q", params["resume_url"])
	}
}

func TestSuspendNotifier_DisabledWhenNoTask(t *testing.T) {
	fired := false
	n := &suspendNotifier{
		notifyTask: "",
		fire:       func(string, map[string]string) error { fired = true; return nil },
		log:        zap.NewNop(),
	}
	n.onRunFinished("t", "r", string(registry.StatusSuspended), "manual", 0)
	if fired {
		t.Error("empty notifyTask must not fire")
	}
}

func TestSuspendNotifier_SkipsOwnRuns(t *testing.T) {
	fired := false
	n := &suspendNotifier{
		notifyTask: "buildin/notifications",
		fire:       func(string, map[string]string) error { fired = true; return nil },
		log:        zap.NewNop(),
	}
	// The notify task's own run must not notify (would loop / self-spam).
	n.onRunFinished("buildin/notifications", "r", string(registry.StatusSuspended), "manual", 0)
	if fired {
		t.Error("notify task's own run must not fire a notification")
	}
}

func TestSuspendNotifier_EndOnlyForResumeContinuation(t *testing.T) {
	done := make(chan map[string]string, 1)
	n := &suspendNotifier{
		notifyTask: "buildin/notifications",
		fire:       func(_ string, p map[string]string) error { done <- p; return nil },
		log:        zap.NewNop(),
	}
	// A resume continuation reaching a terminal state → conversation ended.
	n.onRunFinished("t", "run-2", string(registry.StatusSuccess), string(registry.TriggerResume), 5)
	p := <-done
	if p["event"] != "ended" || p["status"] != string(registry.StatusSuccess) {
		t.Errorf("end params wrong: %+v", p)
	}
}

func TestSuspendNotifier_NoEndForChainOrOneShot(t *testing.T) {
	// Chain children and pipeline stages inherit root != self but are NOT
	// conversation ends; only a resume-sourced terminal run is.
	for _, src := range []string{"manual", "chain", "pipeline", "cron", "webhook"} {
		fired := make(chan struct{}, 1)
		n := &suspendNotifier{
			notifyTask: "buildin/notifications",
			fire:       func(string, map[string]string) error { fired <- struct{}{}; return nil },
			log:        zap.NewNop(),
		}
		n.onRunFinished("t", "run-x", string(registry.StatusSuccess), src, 3)
		select {
		case <-fired:
			t.Errorf("trigger source %q must not fire a conversation-end notification", src)
		default:
		}
	}
}
