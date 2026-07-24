package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// referenceSig independently recomputes the HMAC scheme (crypto/hmac +
// crypto/sha256 directly, not pkg/webhooksign) so these tests actually pin
// the CLI's output against the wire format, not against the package it's
// built on.
func referenceSig(secret, tsStr string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	if tsStr != "" {
		mac.Write([]byte(tsStr))
		mac.Write([]byte("\n"))
	}
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// whatever it wrote, plus fn's error.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	fnErr := fn()
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String(), fnErr
}

// parseSignHeaders splits `dicode webhook sign`'s stdout into its two
// possible header values.
func parseSignHeaders(t *testing.T, out string) (sig string, ts string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed header line %q", line)
		}
		switch parts[0] {
		case "X-Hub-Signature-256":
			sig = parts[1]
		case "X-Dicode-Timestamp":
			ts = parts[1]
		default:
			t.Fatalf("unexpected header line %q", line)
		}
	}
	return sig, ts
}

func TestCmdWebhookSign_NoTimestamp(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return cmdWebhookSign([]string{"--secret", "s3cr3t", "--data", `{"a":1}`, "--no-timestamp"})
	})
	if err != nil {
		t.Fatalf("cmdWebhookSign: %v", err)
	}
	sig, ts := parseSignHeaders(t, out)
	if ts != "" {
		t.Fatalf("expected no X-Dicode-Timestamp header with --no-timestamp, got %q", ts)
	}
	want := referenceSig("s3cr3t", "", []byte(`{"a":1}`))
	if sig != want {
		t.Fatalf("signature mismatch: got %q want %q", sig, want)
	}
}

func TestCmdWebhookSign_ExplicitTimestamp(t *testing.T) {
	out, err := captureStdout(t, func() error {
		return cmdWebhookSign([]string{"--secret", "s3cr3t", "--data", `{"a":1}`, "--timestamp", "1700000000"})
	})
	if err != nil {
		t.Fatalf("cmdWebhookSign: %v", err)
	}
	sig, ts := parseSignHeaders(t, out)
	if ts != "1700000000" {
		t.Fatalf("got timestamp %q, want 1700000000", ts)
	}
	want := referenceSig("s3cr3t", "1700000000", []byte(`{"a":1}`))
	if sig != want {
		t.Fatalf("signature mismatch: got %q want %q", sig, want)
	}
}

func TestCmdWebhookSign_DefaultTimestampIsCurrentTime(t *testing.T) {
	before := time.Now().Unix()
	out, err := captureStdout(t, func() error {
		return cmdWebhookSign([]string{"--secret", "s3cr3t", "--data", "body"})
	})
	after := time.Now().Unix()
	if err != nil {
		t.Fatalf("cmdWebhookSign: %v", err)
	}
	_, ts := parseSignHeaders(t, out)
	if ts == "" {
		t.Fatalf("expected a default X-Dicode-Timestamp header, got none")
	}
	n, convErr := strconv.ParseInt(ts, 10, 64)
	if convErr != nil {
		t.Fatalf("timestamp %q not an integer: %v", ts, convErr)
	}
	if n < before || n > after {
		t.Fatalf("default timestamp %d not within [%d, %d]", n, before, after)
	}
}

func TestCmdWebhookSign_DataFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/body.json"
	if err := os.WriteFile(path, []byte(`{"from":"file"}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out, err := captureStdout(t, func() error {
		return cmdWebhookSign([]string{"--secret", "s3cr3t", "--data-file", path, "--no-timestamp"})
	})
	if err != nil {
		t.Fatalf("cmdWebhookSign: %v", err)
	}
	sig, _ := parseSignHeaders(t, out)
	want := referenceSig("s3cr3t", "", []byte(`{"from":"file"}`))
	if sig != want {
		t.Fatalf("signature mismatch: got %q want %q", sig, want)
	}
}

func TestCmdWebhookSign_Stdin(t *testing.T) {
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdin = r
	go func() {
		_, _ = w.Write([]byte(`{"from":"stdin"}`))
		w.Close()
	}()
	defer func() { os.Stdin = oldStdin }()

	out, err := captureStdout(t, func() error {
		return cmdWebhookSign([]string{"--secret", "s3cr3t", "--no-timestamp"})
	})
	if err != nil {
		t.Fatalf("cmdWebhookSign: %v", err)
	}
	sig, _ := parseSignHeaders(t, out)
	want := referenceSig("s3cr3t", "", []byte(`{"from":"stdin"}`))
	if sig != want {
		t.Fatalf("signature mismatch: got %q want %q", sig, want)
	}
}

func TestCmdWebhookSign_Errors(t *testing.T) {
	unreadableDir := t.TempDir() // a directory path passed as --data-file: not readable as a file body

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing secret", []string{"--data", "x"}, "--secret is required"},
		{"data and data-file", []string{"--secret", "s", "--data", "x", "--data-file", "y"}, "mutually exclusive"},
		{"timestamp and no-timestamp", []string{"--secret", "s", "--data", "x", "--timestamp", "1", "--no-timestamp"}, "mutually exclusive"},
		{"invalid timestamp", []string{"--secret", "s", "--data", "x", "--timestamp", "not-a-number"}, "invalid --timestamp"},
		{"unreadable data-file", []string{"--secret", "s", "--data-file", unreadableDir}, "read --data-file"},
		{"unknown flag", []string{"--secret", "s", "--bogus"}, "unknown flag"},
		{"secret requires value", []string{"--secret"}, "requires a value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := captureStdout(t, func() error {
				return cmdWebhookSign(tc.args)
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestCmdWebhook_UnknownSubcommand(t *testing.T) {
	if err := cmdWebhook([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "unknown webhook subcommand") {
		t.Fatalf("got %v, want unknown-subcommand error", err)
	}
	if err := cmdWebhook(nil); err == nil {
		t.Fatalf("expected error for empty args")
	}
}
