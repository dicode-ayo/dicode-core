package python

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// TestPythonSDK runs the unittest suite for the Python task SDK shim.
// Skips locally when the interpreter is missing; on CI it fails loudly so a
// workflow regression that drops the setup-python step gets caught.
func TestPythonSDK(t *testing.T) {
	py, err := lookPython()
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("python3 not on PATH in CI; setup-python step missing? %v", err)
		}
		t.Skip("python3 not on PATH; skipping Python SDK tests")
	}
	cmd := exec.Command(py, "-m", "unittest", "-v", "test_dicode_sdk")
	cmd.Dir = "sdk"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python sdk tests failed: %v\n%s", err, out)
	}
	t.Logf("python sdk tests output:\n%s", out)
}

// lookPython finds the interpreter that runs the SDK suite. The Windows
// installers (and actions/setup-python) ship "python"; every other platform
// ships "python3", which also shadows a Python 2 "python" where one survives.
func lookPython() (string, error) {
	py, err := exec.LookPath("python3")
	if err == nil {
		return py, nil
	}
	if runtime.GOOS == "windows" {
		if winPy, winErr := exec.LookPath("python"); winErr == nil {
			return winPy, nil
		}
	}
	return "", err
}
