package taskset_test

import (
	"os"
	"strings"
	"testing"
)

func TestAutoFixSkill_FileExists(t *testing.T) {
	path := "../../tasks/skills/dicode-auto-fix.md"
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	// The skill names the tools the model is handed, not the SDK methods
	// behind them: a model cannot call the SDK, so an SDK name in the skill
	// describes a capability the reader does not have.
	required := []string{
		"dicode_get_run_input",
		"dicode_pin_run_input",
		"dicode_unpin_run_input",
		"dicode_set_dev_mode",
		"dicode_git_commit_push",
		"dicode_test_task",
		"dicode_replay_run",
		"max_iterations",
	}
	for _, r := range required {
		if !strings.Contains(string(body), r) {
			t.Errorf("skill missing required term %q", r)
		}
	}
	for _, forbidden := range []string{"dicode.runs.", "dicode.tasks.", "dicode.sources.", "dicode.git.", "Deno.readTextFile", "Deno.writeTextFile"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("skill names %q, which the model has no way to call", forbidden)
		}
	}
}
