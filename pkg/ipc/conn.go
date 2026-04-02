package ipc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const maxMessageSize = 8 * 1024 * 1024 // 8 MiB — guards against runaway allocations

// writeMsg encodes v as JSON and writes it with a 4-byte little-endian length prefix.
func writeMsg(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ipc: marshal: %w", err)
	}
	if len(data) > maxMessageSize {
		return fmt.Errorf("ipc: outbound message too large (%d bytes)", len(data))
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("ipc: write header: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("ipc: write body: %w", err)
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
