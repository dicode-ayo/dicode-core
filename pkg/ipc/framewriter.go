package ipc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// encodeFrame marshals v as JSON behind a 4-byte little-endian length prefix.
func encodeFrame(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ipc: marshal: %w", err)
	}
	if len(data) > maxMessageSize {
		return nil, fmt.Errorf("%w (%d bytes)", ErrMessageTooLarge, len(data))
	}
	buf := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(data)))
	copy(buf[4:], data)
	return buf, nil
}

// frameWriter is the single writer for a connection shared by several
// goroutines: each frame goes out as one Write under one lock, so frames
// never interleave on the wire.
type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newFrameWriter(w io.Writer) *frameWriter { return &frameWriter{w: w} }

func (f *frameWriter) writeMsg(v any) error {
	buf, err := encodeFrame(v)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.w.Write(buf); err != nil {
		return fmt.Errorf("ipc: write: %w", err)
	}
	return nil
}
