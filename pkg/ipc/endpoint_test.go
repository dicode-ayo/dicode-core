package ipc

import (
	"os"
	"testing"
)

// TestIsLoopbackAddr: the predicate separates a loopback endpoint address from
// a Unix-socket path on every platform, since it is what the runtimes branch on
// when deriving the task sandbox's grant for the endpoint.
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:52341", true},
		{"127.0.0.1:0", true},
		// A Unix socket path, as listenIPC builds it.
		{"/tmp/dicode-8e0c/ipc.sock", false},
		// A Windows path: a drive letter is not a host.
		{`C:\Users\x\AppData\Local\Temp\ipc.sock`, false},
		// Any other interface is not an endpoint this daemon creates: binding
		// one would expose the run off-host, so it must not read as loopback.
		{"0.0.0.0:52341", false},
		{"localhost:52341", false},
		{"[::1]:52341", false},
		{"127.0.0.1", false},
		{"127.0.0.1:ipc", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsLoopbackAddr(c.addr); got != c.want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// TestListenIPC_AddrMatchesPredicate: whatever transport the platform builds,
// the address it reports must agree with IsLoopbackAddr about which one it is —
// the runtimes grant the task sandbox access on that answer alone.
func TestListenIPC_AddrMatchesPredicate(t *testing.T) {
	l, addr, dir, err := listenIPC("endpoint-test")
	if err != nil {
		t.Fatalf("listenIPC: %v", err)
	}
	defer l.Close()

	if IsLoopbackAddr(addr) {
		if dir != "" {
			t.Errorf("loopback endpoint reported a directory to clean up: %q", dir)
		}
		if l.Addr().Network() != "tcp" {
			t.Errorf("loopback address %q on a %q listener", addr, l.Addr().Network())
		}
		return
	}

	defer func() { _ = os.RemoveAll(dir) }()
	if dir == "" {
		t.Error("Unix-socket endpoint reported no directory to clean up")
	}
	if l.Addr().Network() != "unix" {
		t.Errorf("path address %q on a %q listener", addr, l.Addr().Network())
	}
}
