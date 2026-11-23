package ipc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// frameWriteTimeout is how long a write may make no progress on a connection
// that supports write deadlines before it fails, so a task that stops reading
// cannot hold the writer forever while a slow reader still gets large frames.
const frameWriteTimeout = 2 * time.Minute

// frameWriteChunk is the most a deadline-bounded write sends per Write; the
// deadline is renewed before each chunk.
const frameWriteChunk = 64 * 1024

// encodeFrame marshals v as JSON behind a 4-byte little-endian length prefix.
// The JSON bytes are identical to json.Marshal's.
func encodeFrame(v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0})
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("ipc: marshal: %w", err)
	}
	b := buf.Bytes()
	b = b[:len(b)-1] // Encode appends '\n'
	n := len(b) - 4
	if n > maxMessageSize {
		return nil, fmt.Errorf("%w (%d bytes)", ErrMessageTooLarge, n)
	}
	binary.LittleEndian.PutUint32(b[:4], uint32(n))
	return b, nil
}

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

// frameWriter is the single writer for a connection shared by several
// goroutines: each frame goes out under one lock, so frames never interleave
// on the wire. Without write deadlines a frame is a single Write.
type frameWriter struct {
	mu      sync.Mutex
	w       io.Writer
	timeout time.Duration
}

func newFrameWriter(w io.Writer) *frameWriter {
	return &frameWriter{w: w, timeout: frameWriteTimeout}
}

func (f *frameWriter) writeMsg(v any) error {
	buf, err := encodeFrame(v)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.w.(writeDeadliner)
	if !ok || f.timeout <= 0 {
		if _, err := f.w.Write(buf); err != nil {
			return fmt.Errorf("ipc: write: %w", err)
		}
		return nil
	}
	defer func() { _ = d.SetWriteDeadline(time.Time{}) }()
	for len(buf) > 0 {
		if err := d.SetWriteDeadline(time.Now().Add(f.timeout)); err != nil {
			return fmt.Errorf("ipc: write: %w", err)
		}
		n, err := f.w.Write(buf[:min(len(buf), frameWriteChunk)])
		if err != nil {
			return fmt.Errorf("ipc: write: %w", err)
		}
		buf = buf[n:]
	}
	return nil
}

// closeOnWriteFailure wraps fw.writeMsg so a failed write closes c: a Write
// that fails may have sent part of a frame, leaving the stream unreadable.
// ErrMessageTooLarge is raised before any byte is written and leaves c open.
func closeOnWriteFailure(fw *frameWriter, c io.Closer) func(any) error {
	return func(v any) error {
		err := fw.writeMsg(v)
		if err != nil && !errors.Is(err, ErrMessageTooLarge) {
			_ = c.Close()
		}
		return err
	}
}
