package ipc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// frameWriteTimeout is how long a gateway push may make no progress on a
// connection that supports write deadlines before it fails, so a task that
// stops reading cannot hold an HTTP caller forever while a slow reader still
// gets large frames.
const frameWriteTimeout = 2 * time.Minute

// frameWriteChunk is the most a deadline-bounded write sends per Write; the
// deadline is renewed before each chunk.
const frameWriteChunk = 64 * 1024

// errFramePartial marks a write that failed after sending part of its frame,
// leaving the stream unreadable.
var errFramePartial = errors.New("ipc: frame partially written")

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
// on the wire.
type frameWriter struct {
	sem     chan struct{} // one-slot lock; a bounded write can give up waiting for it
	w       io.Writer
	timeout time.Duration
}

func newFrameWriter(w io.Writer) *frameWriter {
	return &frameWriter{sem: make(chan struct{}, 1), w: w, timeout: frameWriteTimeout}
}

// writeMsg sends a frame in a single Write with no deadline: a task that has
// exited closes its socket, which fails the Write.
func (f *frameWriter) writeMsg(v any) error {
	buf, err := encodeFrame(v)
	if err != nil {
		return err
	}
	f.sem <- struct{}{}
	defer func() { <-f.sem }()
	if _, err := f.w.Write(buf); err != nil {
		return fmt.Errorf("ipc: write: %w", err)
	}
	return nil
}

// writeMsgBounded sends a frame that fails if it cannot take the lock within
// f.timeout or, when the writer supports write deadlines, once the peer
// accepts no bytes for f.timeout. A failure after part
// of the frame went out wraps errFramePartial.
func (f *frameWriter) writeMsgBounded(v any) error {
	buf, err := encodeFrame(v)
	if err != nil {
		return err
	}
	if f.timeout > 0 {
		t := time.NewTimer(f.timeout)
		select {
		case f.sem <- struct{}{}:
			t.Stop()
		case <-t.C:
			return fmt.Errorf("ipc: write: waiting for the connection: %w", os.ErrDeadlineExceeded)
		}
	} else {
		f.sem <- struct{}{}
	}
	defer func() { <-f.sem }()
	d, ok := f.w.(writeDeadliner)
	if !ok || f.timeout <= 0 {
		return writeErr(f.w.Write(buf))
	}
	defer func() { _ = d.SetWriteDeadline(time.Time{}) }()
	sent := 0
	for sent < len(buf) {
		if err := d.SetWriteDeadline(time.Now().Add(f.timeout)); err != nil {
			return writeErr(sent, err)
		}
		n, err := f.w.Write(buf[sent:min(len(buf), sent+frameWriteChunk)])
		sent += n
		if err != nil {
			return writeErr(sent, err)
		}
	}
	return nil
}

func writeErr(sent int, err error) error {
	switch {
	case err == nil:
		return nil
	case sent > 0:
		return fmt.Errorf("ipc: write: %w: %w", errFramePartial, err)
	default:
		return fmt.Errorf("ipc: write: %w", err)
	}
}

// closeOnWriteFailure wraps fw.writeMsgBounded so a failed write closes c
// unless the stream is still in sync: an oversized frame or a push that timed
// out before sending any byte leaves c open.
func closeOnWriteFailure(fw *frameWriter, c io.Closer) func(any) error {
	return func(v any) error {
		err := fw.writeMsgBounded(v)
		if err != nil && !streamIntact(err) {
			_ = c.Close()
		}
		return err
	}
}

func streamIntact(err error) bool {
	if errors.Is(err, ErrMessageTooLarge) {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, errFramePartial)
}
