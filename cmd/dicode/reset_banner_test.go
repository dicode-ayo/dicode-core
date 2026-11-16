package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrintResetBanner_Aligned(t *testing.T) {
	value := strings.Repeat("x", 43)
	out, _ := captureStdout(t, func() error {
		printResetBanner(value)
		return nil
	})

	if !strings.Contains(out, value) {
		t.Fatalf("passphrase missing from banner:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := utf8.RuneCountInString(lines[0])
	for _, line := range lines {
		if got := utf8.RuneCountInString(line); got != want {
			t.Errorf("banner line width = %d; want %d (top border): %q", got, want, line)
		}
	}
}
