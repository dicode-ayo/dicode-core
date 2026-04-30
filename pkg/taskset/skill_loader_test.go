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
	required := []string{
		"dicode.runs.get_input",
		"dicode.runs.pin_input",
		"dicode.runs.unpin_input",
		"dicode.sources.set_dev_mode",
		"dicode.git.commit_push",
		"dicode.tasks.test",
		"dicode.runs.replay",
		"max_iterations",
		"defer cleanup",
	}
	for _, r := range required {
		if !strings.Contains(string(body), r) {
			t.Errorf("skill missing required term %q", r)
		}
	}
}
