package onboarding

import (
	"bytes"
	"strings"
	"testing"
)

const testHome = "/home/testuser"

// scriptedStdin builds stdin from a list of lines, each terminated by \n.
func scriptedStdin(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

func TestRunCLI_AllDefaults_AllPresetsOn(t *testing.T) {
	// Empty responses = accept every default: all presets on, default dirs,
	// port 8080, skip advanced. Passphrase is always generated.
	in := scriptedStdin(
		"", // buildin default (y)
		"", // examples default (y)
		"", // auth default (y)
		"", // local tasks dir default
		"", // advanced? default (n)
	)
	res, err := RunCLI(in, &bytes.Buffer{}, testHome)
	if err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	for _, p := range TaskSetPresets {
		if !res.TaskSetsEnabled[p.Name] {
			t.Errorf("preset %q should be enabled", p.Name)
		}
	}
	if res.LocalTasksDir != testHome+"/dicode-tasks" {
		t.Errorf("LocalTasksDir = %q; want %s/dicode-tasks", res.LocalTasksDir, testHome)
	}
	if res.DataDir != testHome+"/.dicode" {
		t.Errorf("DataDir = %q; want %s/.dicode", res.DataDir, testHome)
	}
	if res.Port != 8080 {
		t.Errorf("Port = %d; want 8080", res.Port)
	}
	if len(res.Passphrase) != 24 {
		t.Errorf("Passphrase len = %d; want 24", len(res.Passphrase))
	}
}

func TestRunCLI_DisableExamples(t *testing.T) {
	in := scriptedStdin(
		"y", // buildin
		"n", // examples disabled
		"y", // auth
		"",  // local dir default
		"",  // advanced no
	)
	res, err := RunCLI(in, &bytes.Buffer{}, testHome)
	if err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	if res.TaskSetsEnabled["examples"] {
		t.Error("examples should be disabled")
	}
	if !res.TaskSetsEnabled["buildin"] || !res.TaskSetsEnabled["auth"] {
		t.Error("buildin and auth should be enabled")
	}
}

func TestRunCLI_SkipLocalDir(t *testing.T) {
	in := scriptedStdin(
		"y", "y", "y", // all presets
		"skip",         // explicit "skip" → empty LocalTasksDir
		"",             // advanced no
	)
	res, err := RunCLI(in, &bytes.Buffer{}, testHome)
	if err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	if res.LocalTasksDir != "" {
		t.Errorf("LocalTasksDir = %q; want empty after 'skip'", res.LocalTasksDir)
	}
}

func TestRunCLI_CustomLocalDir(t *testing.T) {
	in := scriptedStdin(
		"y", "y", "y",
		"/opt/tasks",
		"",
	)
	res, err := RunCLI(in, &bytes.Buffer{}, testHome)
	if err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	if res.LocalTasksDir != "/opt/tasks" {
		t.Errorf("LocalTasksDir = %q; want /opt/tasks", res.LocalTasksDir)
	}
}

func TestRunCLI_AdvancedOverrides(t *testing.T) {
	in := scriptedStdin(
		"y", "y", "y",
		"",             // local dir default
		"y",            // advanced? yes
		"/var/dicode",  // data dir
		"9090",         // port
	)
	res, err := RunCLI(in, &bytes.Buffer{}, testHome)
	if err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	if res.DataDir != "/var/dicode" {
		t.Errorf("DataDir = %q; want /var/dicode", res.DataDir)
	}
	if res.Port != 9090 {
		t.Errorf("Port = %d; want 9090", res.Port)
	}
}

func TestRunCLI_ExplicitAcceptCapitalY(t *testing.T) {
	in := scriptedStdin(
		"Y", "Y", "Y",
		"",
		"",
	)
	res, err := RunCLI(in, &bytes.Buffer{}, testHome)
	if err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	for _, p := range TaskSetPresets {
		if !res.TaskSetsEnabled[p.Name] {
			t.Errorf("preset %q should be enabled with 'Y'", p.Name)
		}
	}
}
