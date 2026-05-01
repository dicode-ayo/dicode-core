//go:build !windows

package python

import (
	"os"
	"os/exec"
	"testing"
)

// TestPythonSDK runs the unittest suite for the Python task SDK shim.
// The SDK speaks a Unix-socket IPC protocol, so the suite is Unix-only.
// Skips locally when python3 is missing; on CI it fails loudly so a
// workflow regression that drops the setup-python step gets caught.
func TestPythonSDK(t *testing.T) {
	py, err := exec.LookPath("python3")
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
