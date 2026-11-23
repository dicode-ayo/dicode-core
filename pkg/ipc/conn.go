package ipc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxMessageSize = 8 * 1024 * 1024 // 8 MiB — guards against runaway allocations

// ErrMessageTooLarge is returned by writeMsg when the marshaled message exceeds
// maxMessageSize. The connection is still healthy — callers can reply with an
// error to the specific request instead of tearing the connection down.
var ErrMessageTooLarge = errors.New("ipc: outbound message too large")

// writeMsg encodes v as JSON and writes it with a 4-byte little-endian length
// prefix, as a single Write.
func writeMsg(w io.Writer, v any) error {
	buf, err := encodeFrame(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("ipc: write: %w", err)
	}
	return nil
}

// readMsg reads a length-prefixed message and decodes it into v.
func readMsg(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("ipc: read header: %w", err)
	}
	size := binary.LittleEndian.Uint32(hdr[:])
	if size == 0 {
		return fmt.Errorf("ipc: zero-length message")
	}
	if size > maxMessageSize {
		return fmt.Errorf("ipc: message too large (%d bytes)", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("ipc: read body: %w", err)
	}
	if err := json.Unmarshal(buf, v); err != nil {
		return fmt.Errorf("ipc: unmarshal: %w", err)
	}
	return nil
}
