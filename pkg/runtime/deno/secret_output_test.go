package deno

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRun_SecretOutputRoutedToChannel runs a real Deno task that calls
// `dicode.output({PG_URL:"postgres://x"}, { secret: true })` and asserts
// the daemon's secret-output channel sees the map.
//
// Skipped in -short mode because it spawns an actual Deno subprocess.
func TestRun_SecretOutputRoutedToChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rt, reg, cleanup := newTestRuntime(t)
	defer cleanup()

	spec := writeProviderTask(t, "doppler", `
export default async function main({ output }) {
  await output({ PG_URL: "postgres://x" }, { secret: true });
}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	out := make(chan map[string]string, 1)
	rt.SetSecretOutputChannel(out)

	res, err := rt.Run(ctx, spec, RunOptions{RunID: "run-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("run error: %v", res.Error)
	}

	select {
	case got := <-out:
		if got["PG_URL"] != "postgres://x" {
			t.Errorf("got = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("secret output not routed")
	}

	logs, _ := reg.GetRunLogs(ctx, "run-1")
	for _, l := range logs {
		if strings.Contains(l.Message, "postgres://x") {
			t.Errorf("plaintext leaked: %q", l.Message)
		}
	}
}
