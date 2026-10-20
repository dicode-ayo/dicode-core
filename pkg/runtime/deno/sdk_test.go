package deno

import (
	"os"
	"os/exec"
	"testing"

	denopkg "github.com/dicode/dicode/pkg/deno"
)

// TestDenoSDK runs the Deno test suite for the SDK shim. A Deno provisioning
// failure skips locally but fails on CI, so a broken setup cannot silently
// drop the suite.
func TestDenoSDK(t *testing.T) {
	denoPath, err := denopkg.EnsureDeno(denopkg.DefaultVersion)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("deno not available in CI: %v", err)
		}
		t.Skipf("deno not available: %v", err)
	}
	cmd := exec.Command(denoPath, "test", "--allow-all", "--no-lock", "sdk/shim_test.ts")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deno sdk tests failed: %v\n%s", err, out)
	}
	t.Logf("deno sdk tests output:\n%s", out)
}
