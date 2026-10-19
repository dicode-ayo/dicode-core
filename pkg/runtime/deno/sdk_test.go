package deno

import (
	"os"
	"os/exec"
	"testing"
)

// TestDenoSDK runs the Deno test suite for the SDK shim. Skips locally when
// Deno is unavailable; on CI it fails loudly.
func TestDenoSDK(t *testing.T) {
	rt, _, cleanup := newTestRuntime(t)
	defer cleanup()
	denoPath := rt.denoPath
	if denoPath == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("deno binary not resolved in CI")
		}
		t.Skip("deno not available")
	}
	cmd := exec.Command(denoPath, "test", "--allow-all", "--no-lock", "sdk/shim_test.ts")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deno sdk tests failed: %v\n%s", err, out)
	}
	t.Logf("deno sdk tests output:\n%s", out)
}
