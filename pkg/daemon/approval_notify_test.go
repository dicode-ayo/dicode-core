package daemon

import (
	"errors"
	"strings"
	"testing"
)

type fireCall struct {
	taskID string
	params map[string]string
}

func TestApprovalNotifyFiresWithParams(t *testing.T) {
	var broadcasts [][2]string
	var fires []fireCall
	n := approvalNotifier{
		notifyTask: "buildin/notify",
		broadcast:  func(id, hash string) { broadcasts = append(broadcasts, [2]string{id, hash}) },
		mintLink:   func(id string) (string, error) { return "https://host/approve/tok123", nil },
		fire: func(id string, params map[string]string) error {
			fires = append(fires, fireCall{id, params})
			return nil
		},
	}

	n.notify("repo/deploy", "abc123")

	if len(broadcasts) != 1 || broadcasts[0] != [2]string{"repo/deploy", "abc123"} {
		t.Fatalf("broadcasts = %v", broadcasts)
	}
	if len(fires) != 1 {
		t.Fatalf("fires = %v, want 1", fires)
	}
	f := fires[0]
	if f.taskID != "buildin/notify" {
		t.Fatalf("fired task = %q, want buildin/notify", f.taskID)
	}
	want := map[string]string{
		"task_id":     "repo/deploy",
		"hash":        "abc123",
		"approve_url": "https://host/approve/tok123",
		"event":       "approval_pending",
		"title":       "dicode: a task is waiting for approval",
		"priority":    "default",
	}
	for k, v := range want {
		if f.params[k] != v {
			t.Fatalf("param %q = %q, want %q", k, f.params[k], v)
		}
	}
	// buildin/notifications hard-fails on an empty title or body, so a delivery
	// task that composes nothing itself must still be able to send.
	if body := f.params["body"]; !strings.Contains(body, "repo/deploy") ||
		!strings.Contains(body, "https://host/approve/tok123") {
		t.Fatalf("body = %q, want the task ID and the approve link", body)
	}
}

func TestApprovalNotifyBodyOmitsEmptyApproveURL(t *testing.T) {
	var fires []fireCall
	n := approvalNotifier{
		notifyTask: "buildin/notifications",
		mintLink:   func(string) (string, error) { return "", errors.New("not pending") },
		fire: func(id string, params map[string]string) error {
			fires = append(fires, fireCall{id, params})
			return nil
		},
	}

	n.notify("repo/deploy", "abc123")

	if len(fires) != 1 {
		t.Fatalf("fires = %v, want 1", fires)
	}
	body := fires[0].params["body"]
	if body == "" {
		t.Fatal("body is empty; buildin/notifications rejects that")
	}
	if strings.Contains(body, "Approve:") {
		t.Fatalf("body = %q, want no dangling Approve: label when no link was minted", body)
	}
}

func TestApprovalNotifyEmptyNotifyTaskBroadcastsOnly(t *testing.T) {
	var broadcasts int
	var minted, fired bool
	n := approvalNotifier{
		notifyTask: "",
		broadcast:  func(string, string) { broadcasts++ },
		mintLink:   func(string) (string, error) { minted = true; return "", nil },
		fire:       func(string, map[string]string) error { fired = true; return nil },
	}

	n.notify("repo/deploy", "abc123")

	if broadcasts != 1 {
		t.Fatalf("broadcasts = %d, want 1", broadcasts)
	}
	if minted || fired {
		t.Fatalf("empty notify_task must not mint (%v) or fire (%v)", minted, fired)
	}
}

func TestApprovalNotifyMintFailureStillFiresEmptyURL(t *testing.T) {
	var fires []fireCall
	n := approvalNotifier{
		notifyTask: "buildin/notify",
		broadcast:  func(string, string) {},
		mintLink:   func(string) (string, error) { return "", errors.New("not pending") },
		fire: func(id string, params map[string]string) error {
			fires = append(fires, fireCall{id, params})
			return nil
		},
	}

	n.notify("repo/deploy", "abc123")

	if len(fires) != 1 {
		t.Fatalf("fires = %v, want 1 even when mint fails", fires)
	}
	if got := fires[0].params["approve_url"]; got != "" {
		t.Fatalf("approve_url = %q, want empty on mint failure", got)
	}
}

func TestApprovalNotifyFireFailureDoesNotPanic(t *testing.T) {
	n := approvalNotifier{
		notifyTask: "repo/untrusted-notify", // would itself be pending → veto
		broadcast:  func(string, string) {},
		mintLink:   func(string) (string, error) { return "url", nil },
		fire:       func(string, map[string]string) error { return errors.New("task pending approval") },
	}
	// Must not panic; failure is swallowed (logged).
	n.notify("repo/deploy", "abc123")
}

func TestApprovalNotifyNilDepsNoPanic(t *testing.T) {
	// Degrade gracefully if any dependency is nil (defensive against wiring
	// order / partial construction).
	(approvalNotifier{notifyTask: "buildin/notify"}).notify("repo/deploy", "h")
	(approvalNotifier{}).notify("repo/deploy", "h")
}
