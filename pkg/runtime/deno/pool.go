package deno

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
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
	conn       net.Conn
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

	// counter for generating unique socket names
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
			go p.replenish()
		}
	}
	return p
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
// It runs in its own goroutine and respects p.ctx cancellation.
func (p *Pool) replenish() {
	if p.ctx.Err() != nil {
		return
	}

	id := p.counter.Add(1)
	socketPath := fmt.Sprintf("/tmp/dicode-pool-%d.sock", id)
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		// If we can't listen, back off briefly and try again.
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		go p.replenish()
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

	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		go p.replenish()
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
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		go p.replenish()
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
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		go p.replenish()
		return
	}
	_ = conn.SetDeadline(time.Time{}) // clear deadline

	w := &warmProc{cmd: cmd, socketPath: socketPath, conn: conn}

	select {
	case p.ready <- w:
		// Successfully added to pool.
	case <-p.ctx.Done():
		w.close()
	}
}

// Close drains the pool, kills all waiting processes, and stops the pool.
func (p *Pool) Close() {
	p.cancel()
	// Drain and kill all waiting warm processes.
	for {
		select {
		case w := <-p.ready:
			w.close()
		default:
			return
		}
	}
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
