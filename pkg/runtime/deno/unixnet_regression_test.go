//go:build !windows

package deno

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	denopkg "github.com/dicode/dicode/pkg/deno"
	"github.com/dicode/dicode/pkg/task"
)

// TestUnixNetPermission_Regression starts a real Unix-domain socket listener
// and runs a real Deno 2.9.6 binary against the exact argv buildDenoArgs
// produces for a Unix-socket IPC transport, proving
// Deno.connect({transport:"unix"}) actually succeeds under the emitted
// --allow-net=unix:<path> grant. This is the gap that let 318 green
// (mocked-SDK) tests hide the bug: none of them opened a real socket.
//
// Before the fix (no "unix:" scope emitted for a version that gates it),
// this test fails with a NotCapable permission error. After the fix, it
// passes.
func TestUnixNetPermission_Regression(t *testing.T) {
	denoBin, err := denopkg.EnsureDeno("2.9.6")
	if err != nil {
		t.Skipf("could not obtain deno 2.9.6 (no network?): %v", err)
	}

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "i.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer ln.Close()

	connected := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		connected <- struct{}{}
		conn.Close()
	}()

	scriptPath := filepath.Join(dir, "connect.ts")
	script := `
try {
  const conn = await Deno.connect({ transport: "unix", path: Deno.args[0] });
  conn.close();
  Deno.exit(0);
} catch (e) {
  console.error(String(e));
  Deno.exit(1);
}
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	spec := &task.Spec{
		ID:      "unixnet-regression",
		Name:    "unixnet-regression",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 30 * time.Second,
		TaskDir: dir,
	}
	// Build the same argv shape buildDenoArgs would produce for a Unix-socket
	// IPC endpoint with no declared task permissions, against Deno 2.9.6 —
	// this is what exercises the --allow-net=unix:<path> flag the fix emits.
	args := buildDenoArgs(spec, sockPath, scriptPath, scriptPath, nil, "2.9.6")
	// buildDenoArgs's --allow-read already covers scriptPath (it doubles as
	// both "shimPath" and "runnerPath" here) and sockPath; append the script
	// args (the socket path, as Deno.args[0]).
	args = append(args, sockPath)

	cmd := exec.Command(denoBin, args...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deno run failed: %v\noutput:\n%s\nargs: %v", err, out, args)
	}

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("listener never saw a connection")
	}
}
