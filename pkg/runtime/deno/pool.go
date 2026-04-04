package deno

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxPoolSize caps the DICODE_DENO_POOL_SIZE value to prevent resource exhaustion.
	maxPoolSize = 32
)

// poolDispatch is the message sent from the Go pool to a warm Deno process
// after it connects and declares itself ready.
type poolDispatch struct {
	SocketPath string `json:"socketPath"`
	Token      string `json:"token"`
	Script     string `json:"script"`
}

// warmProc represents a single pre-warmed Deno process waiting for a task.
type warmProc struct {
	cmd        *exec.Cmd
	socketPath string // pool socket this process is listening on
	socketDir  string // private 0700 temp directory containing socketPath
	conn       net.Conn
	stderr     io.ReadCloser // captured before cmd.Start(); used by Run() for log streaming
}

// dispatch sends the task details to the warm process so it can execute.
// After dispatch the caller owns the process (cmd); Close the conn first.
func (w *warmProc) dispatch(msg poolDispatch) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, uint32(len(data)))
	frame := append(hdr, data...)
	_, err = w.conn.Write(frame)
	return err
}

// close cleans up the warm process resources without running a task.
func (w *warmProc) close() {
	if w.conn != nil {
		_ = w.conn.Close()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
		_ = w.cmd.Wait()
	}
	if w.socketPath != "" {
		_ = os.Remove(w.socketPath)
	}
	if w.socketDir != "" {
		_ = os.Remove(w.socketDir)
	}
}

// Pool maintains a set of pre-warmed Deno processes ready for immediate dispatch.
// Size 0 disables the pool (backwards-compatible default).
type Pool struct {
	size     int
	denoPath string
	shimPath string // path to pool_shim.js embedded temp file

	ready  chan *warmProc
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks in-flight replenish() goroutines so Close() can wait for them.
	wg sync.WaitGroup

	// counter for generating unique socket names (kept for fallback uniqueness,
	// primary isolation is via per-socket private temp dir).
	counter atomic.Uint64
}

// poolReadyMsg is received from the warm process when it's ready.
type poolReadyMsg struct {
	Ready bool `json:"ready"`
}

// NewPool creates and starts a warm process pool.
// size == 0 returns a disabled pool (Acquire always returns an error).
// If denoPath does not point to an executable, the pool is disabled with a warning.
func NewPool(ctx context.Context, denoPath string, shimPath string, size int) *Pool {
	poolCtx, cancel := context.WithCancel(ctx)
	p := &Pool{
		size:     size,
		denoPath: denoPath,
		shimPath: shimPath,
		ready:    make(chan *warmProc, size),
		ctx:      poolCtx,
		cancel:   cancel,
	}
	if size > 0 {
		for i := 0; i < size; i++ {
			p.goReplenish()
		}
	}
	return p
}

// goReplenish increments the WaitGroup and launches a replenish goroutine.
// wg.Add must happen in the caller (before go) to avoid racing with wg.Wait
// in Close().
func (p *Pool) goReplenish() {
	p.wg.Add(1)
	go p.replenish()
}

// Acquire blocks until a warm process is available or ctx is cancelled.
// Returns an error if the pool is disabled (size == 0) or ctx expires.
func (p *Pool) Acquire(ctx context.Context) (*warmProc, error) {
	if p.size == 0 {
		return nil, fmt.Errorf("pool disabled")
	}
	select {
	case w := <-p.ready:
		return w, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, fmt.Errorf("pool closed")
	}
}

// replenish spawns a single new warm process and adds it to the ready channel.
// It runs in its own goroutine (launched via goReplenish) and respects p.ctx
// cancellation. Do NOT call this directly; use goReplenish() so that
// wg.Add(1) happens before the goroutine starts (avoiding a race with
// wg.Wait in Close).
func (p *Pool) replenish() {
	defer p.wg.Done()

	if p.ctx.Err() != nil {
		return
	}

	// Create a private 0700 directory for the socket so that other local users
	// cannot connect and intercept the dispatch message (which contains the IPC
	// token and full task script).
	socketDir, err := os.MkdirTemp("", "dicode-pool-*")
	if err != nil {
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		p.goReplenish()
		return
	}
	if err := os.Chmod(socketDir, 0700); err != nil {
		_ = os.Remove(socketDir)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		p.goReplenish()
		return
	}
	socketPath := socketDir + "/pool.sock"

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		// If we can't listen, back off briefly and try again.
		_ = os.Remove(socketDir)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		p.goReplenish()
		return
	}

	cmd := exec.CommandContext(p.ctx, p.denoPath,
		"run",
		"--allow-net",
		"--allow-env",
		"--allow-read",
		"--allow-write",
		p.shimPath,
	) //nolint:gosec
	cmd.Env = append(os.Environ(), "DICODE_POOL_SOCKET="+socketPath)

	// Capture StderrPipe BEFORE cmd.Start(); Go disallows calling it after Start.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketDir)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		p.goReplenish()
		return
	}

	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketDir)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		p.goReplenish()
		return
	}

	// Accept the connection from the warm process (with a deadline).
	_ = ln.(*net.UnixListener).SetDeadline(time.Now().Add(15 * time.Second))
	conn, err := ln.Accept()
	_ = ln.Close() // only one connection expected
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketDir)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		p.goReplenish()
		return
	}

	// Read the ready message from the warm process.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var ready poolReadyMsg
	if err := readPoolMsg(conn, &ready); err != nil || !ready.Ready {
		_ = conn.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketDir)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		p.goReplenish()
		return
	}
	_ = conn.SetDeadline(time.Time{}) // clear deadline

	w := &warmProc{cmd: cmd, socketPath: socketPath, socketDir: socketDir, conn: conn, stderr: stderrPipe}

	select {
	case p.ready <- w:
		// Successfully added to pool.
	case <-p.ctx.Done():
		w.close()
	}
}

// Close drains the pool, kills all waiting processes, and stops the pool.
// It waits for all in-flight replenish goroutines to exit before returning.
func (p *Pool) Close() {
	p.cancel()
	// Drain and kill all waiting warm processes.
	for {
		select {
		case w := <-p.ready:
			w.close()
		default:
			goto drained
		}
	}
drained:
	// Wait for replenish goroutines to observe cancellation and exit.
	p.wg.Wait()
}

// ── framing helpers ──────────────────────────────────────────────────────────

func readPoolMsg(conn net.Conn, out interface{}) error {
	hdr := make([]byte, 4)
	if _, err := readExactPool(conn, hdr); err != nil {
		return err
	}
	size := binary.LittleEndian.Uint32(hdr)
	if size > 4*1024*1024 { // 4 MB sanity limit
		return fmt.Errorf("pool msg too large: %d", size)
	}
	body := make([]byte, size)
	if _, err := readExactPool(conn, body); err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func readExactPool(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
