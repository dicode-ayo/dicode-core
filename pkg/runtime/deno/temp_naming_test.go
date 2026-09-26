package deno

import (
	"net/url"
	"os"
	"path/filepath"
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

// TestModuleURL_IsAnImportableSpecifier: the runner imports the shim and the
// task script by embedding their paths in a module specifier, so a raw OS path
// will not do — a Windows separator is an escape sequence inside the string
// literal, and a space is invalid in a URL.
func TestModuleURL_IsAnImportableSpecifier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dicode shim__x.ts")
	got := moduleURL(path)

	if !strings.HasPrefix(got, "file:///") {
		t.Errorf("moduleURL(%q) = %q, want an absolute file: URL", path, got)
	}
	if strings.ContainsAny(got, `\ `) {
		t.Errorf("specifier %q carries a raw separator or space", got)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	want := filepath.ToSlash(path)
	if !strings.HasPrefix(want, "/") {
		want = "/" + want
	}
	if u.Path != want {
		t.Errorf("specifier resolves to %q, want %q", u.Path, want)
	}
}
