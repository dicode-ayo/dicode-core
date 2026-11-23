package ipc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// recordingWriter keeps every Write call's bytes separately.
type recordingWriter struct {
	mu     sync.Mutex
	writes [][]byte
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

// assertWholeFrame fails unless p is exactly one complete length-prefixed frame.
func assertWholeFrame(t *testing.T, i int, p []byte) {
	t.Helper()
	r := bytes.NewReader(p)
	var m map[string]any
	if err := readMsg(r, &m); err != nil {
		t.Fatalf("write %d (%d bytes) is not a complete frame: %v", i, len(p), err)
	}
	if r.Len() != 0 {
		t.Fatalf("write %d carries %d trailing bytes after its frame", i, r.Len())
	}
}

func TestWriteMsg_OneWritePerFrame(t *testing.T) {
	rw := &recordingWriter{}
	for i := 0; i < 3; i++ {
		if err := writeMsg(rw, Response{ID: fmt.Sprint(i), Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(rw.writes) != 3 {
		t.Fatalf("expected 3 Write calls for 3 frames, got %d", len(rw.writes))
	}
	for i, p := range rw.writes {
		assertWholeFrame(t, i, p)
	}
}

func TestFrameWriter_OneWritePerFrame(t *testing.T) {
	rw := &recordingWriter{}
	fw := newFrameWriter(rw)
	if err := fw.writeMsg(Response{ID: "1", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := fw.writeMsg(HTTPInboundRequest{RequestID: "r1", HTTPMethod: "GET", Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	if len(rw.writes) != 2 {
		t.Fatalf("expected 2 Write calls, got %d", len(rw.writes))
	}
	for i, p := range rw.writes {
		assertWholeFrame(t, i, p)
	}
}

func TestFrameWriter_TooLargeWritesNothing(t *testing.T) {
	rw := &recordingWriter{}
	fw := newFrameWriter(rw)
	err := fw.writeMsg(Response{ID: "1", Result: strings.Repeat("x", maxMessageSize)})
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("expected ErrMessageTooLarge, got %v", err)
	}
	if len(rw.writes) != 0 {
		t.Fatalf("expected no Write for an oversized frame, got %d", len(rw.writes))
	}
}

func TestEncodeFrame_MatchesJSONMarshal(t *testing.T) {
	v := Response{ID: "<a&b>", Result: map[string]any{"html": "<script>&</script>", "n": 1}}
	want, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err := encodeFrame(v)
	if err != nil {
		t.Fatal(err)
	}
	if n := binary.LittleEndian.Uint32(got[:4]); int(n) != len(want) {
		t.Fatalf("length prefix %d, want %d", n, len(want))
	}
	if !bytes.Equal(got[4:], want) {
		t.Fatalf("frame body %q, want %q", got[4:], want)
	}
}

// A peer that never reads must not hold the writer, or any writer queued
// behind it, forever.
func TestFrameWriter_StalledPeerTimesOut(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	fw := newFrameWriter(a)
	fw.timeout = 100 * time.Millisecond

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) { errs <- fw.writeMsg(Response{ID: fmt.Sprint(i), Result: "ok"}) }(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("writer %d: expected a deadline error, got %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("writer %d still blocked on a peer that never reads", i)
		}
	}
}

// failingConn fails every Write and records Close.
type failingConn struct {
	mu     sync.Mutex
	closed bool
}

func (f *failingConn) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func (f *failingConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func TestCloseOnWriteFailure(t *testing.T) {
	t.Run("write error closes", func(t *testing.T) {
		c := &failingConn{}
		push := closeOnWriteFailure(newFrameWriter(c), c)
		if err := push(Response{ID: "1"}); err == nil {
			t.Fatal("expected write error")
		}
		if !c.closed {
			t.Fatal("connection left open after a failed push")
		}
	})
	t.Run("too large keeps open", func(t *testing.T) {
		c := &failingConn{}
		push := closeOnWriteFailure(newFrameWriter(c), c)
		err := push(Response{ID: "1", Result: strings.Repeat("x", maxMessageSize)})
		if !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("expected ErrMessageTooLarge, got %v", err)
		}
		if c.closed {
			t.Fatal("connection closed for an oversized frame that wrote nothing")
		}
	})
}

// choppyWriter accepts each Write in small chunks and yields between them,
// so any two unserialized writers interleave their bytes.
type choppyWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *choppyWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += 3 {
		end := min(off+3, len(p))
		c.mu.Lock()
		c.buf.Write(p[off:end])
		c.mu.Unlock()
		runtime.Gosched()
	}
	return len(p), nil
}

func TestFrameWriter_ConcurrentWritersDoNotInterleave(t *testing.T) {
	cw := &choppyWriter{}
	fw := newFrameWriter(cw)

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				var msg any = Response{ID: fmt.Sprintf("%d-%d", w, i), Result: strings.Repeat("r", 64)}
				if w%2 == 0 {
					msg = HTTPInboundRequest{RequestID: fmt.Sprintf("%d-%d", w, i), HTTPMethod: "POST", ReqBody: bytes.Repeat([]byte("b"), 64)}
				}
				if err := fw.writeMsg(msg); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	r := bytes.NewReader(cw.buf.Bytes())
	n := 0
	for r.Len() > 0 {
		var m map[string]any
		if err := readMsg(r, &m); err != nil {
			t.Fatalf("frame %d corrupt: %v", n, err)
		}
		n++
	}
	if n != writers*perWriter {
		t.Fatalf("decoded %d frames, want %d", n, writers*perWriter)
	}
}

// Gateway pushes (HTTP goroutines) and SDK replies (read-loop goroutine) share
// one task connection; every frame the task reads must decode intact.
func TestServer_GatewayPushesAndRepliesDoNotInterleave(t *testing.T) {
	g := NewGateway()
	e := newTestEnv(t)

	params := map[string]string{"blob": strings.Repeat("p", 16*1024)}
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	spec := &task.Spec{ID: "race-daemon", Trigger: task.TriggerConfig{Daemon: true}}
	srv := New(runID, "race-daemon", e.secret, e.reg, e.db, params, nil, zap.NewNop(), spec, nil)
	srv.SetGateway(g)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{"id": "reg", "method": "http.register", "pattern": "/hooks/race-daemon"})
	if resp := recvMsg(t, conn); resp["error"] != nil {
		t.Fatalf("http.register error: %v", resp["error"])
	}

	const pushes, calls = 100, 100
	client := newFrameWriter(conn)

	readErr := make(chan error, 1)
	repliesDone := make(chan struct{})
	go func() {
		replies := 0
		for {
			var m map[string]any
			if err := readMsg(conn, &m); err != nil {
				if !errors.Is(err, io.EOF) {
					select {
					case readErr <- err:
					default:
					}
				}
				return
			}
			if rid, ok := m["requestID"].(string); ok {
				go func() {
					_ = client.writeMsg(map[string]any{
						"method": "http.respond", "requestID": rid, "status": 200, "respBody": []byte(rid),
					})
				}()
				continue
			}
			if _, ok := m["id"].(string); !ok {
				select {
				case readErr <- fmt.Errorf("unexpected frame: %v", m):
				default:
				}
				return
			}
			if m["error"] != nil {
				select {
				case readErr <- fmt.Errorf("reply error: %v", m["error"]):
				default:
				}
				return
			}
			replies++
			if replies == calls {
				close(repliesDone)
			}
		}
	}()

	var wg sync.WaitGroup
	codes := make(chan int, pushes)
	for i := 0; i < pushes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := strings.NewReader(strings.Repeat("h", 16*1024))
			req := httptest.NewRequest(http.MethodPost, "/hooks/race-daemon", body)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, req)
			codes <- w.Code
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < calls; i++ {
			if err := client.writeMsg(map[string]any{"id": fmt.Sprint(i), "method": "params"}); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	timeout := time.After(15 * time.Second)
	for _, ch := range []chan struct{}{done, repliesDone} {
		select {
		case <-ch:
		case err := <-readErr:
			t.Fatalf("task-side read desynced: %v", err)
		case <-timeout:
			t.Fatal("timed out waiting for pushes and replies")
		}
	}
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("gateway request returned %d, want 200", c)
		}
	}
}
