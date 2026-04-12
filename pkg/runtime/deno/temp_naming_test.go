package deno

import (
	"os"
	"strings"
	"testing"
)

// TestTempFileNamingEmbedsRunID verifies that os.CreateTemp with the
// "dicode-<kind>-<runID>__*" pattern produces filenames from which the
// run_id can be parsed back out. The buildin/temp-cleanup task relies
// on this naming to match temp files against currently-running runs.
func TestTempFileNamingEmbedsRunID(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		runID  string
	}{
		{"shim with uuid", "dicode-shim-", "550e8400-e29b-41d4-a716-446655440000"},
		{"runner with uuid", "dicode-runner-", "550e8400-e29b-41d4-a716-446655440000"},
		{"task with uuid", "dicode-task-", "550e8400-e29b-41d4-a716-446655440000"},
		{"runID without dashes", "dicode-shim-", "abcdef0123456789"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp("", tc.prefix+tc.runID+"__*.ts")
			if err != nil {
				t.Fatalf("CreateTemp: %v", err)
			}
			name := f.Name()
			_ = f.Close()
			t.Cleanup(func() { _ = os.Remove(name) })

			base := name
			if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
				base = name[idx+1:]
			}
			if !strings.HasPrefix(base, tc.prefix) {
				t.Errorf("name %q missing prefix %q", base, tc.prefix)
			}
			rest := strings.TrimPrefix(base, tc.prefix)
			sep := strings.Index(rest, "__")
			if sep < 0 {
				t.Fatalf("name %q has no __ separator", base)
			}
			got := rest[:sep]
			if got != tc.runID {
				t.Errorf("parsed run_id = %q, want %q (full name %q)", got, tc.runID, base)
			}
		})
	}
}
