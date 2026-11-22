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

// writeMsg encodes v as JSON and writes it with a 4-byte little-endian length prefix.
func writeMsg(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ipc: marshal: %w", err)
	}
	if len(data) > maxMessageSize {
		return fmt.Errorf("%w (%d bytes)", ErrMessageTooLarge, len(data))
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
		return &decodeError{raw: buf, err: err}
	}
	return nil
}

// decodeError is returned by readMsg when a frame was read whole but its JSON
// does not fit the target. Unlike a framing error, the stream is still in sync.
type decodeError struct {
	raw []byte
	err error
}

func (e *decodeError) Error() string { return "ipc: unmarshal: " + e.err.Error() }
func (e *decodeError) Unwrap() error { return e.err }

const maxInvalidReasonLen = 200

// invalidRequest classifies a readMsg error from a request loop. ok is false
// for a framing error, after which the connection must be closed. Otherwise
// id is the frame's string id ("" when absent or not a string) and msg is the
// error text to reply with.
func invalidRequest(err error) (id, msg string, ok bool) {
	var de *decodeError
	if !errors.As(err, &de) {
		return "", "", false
	}
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(de.raw, &probe) == nil {
		_ = json.Unmarshal(probe.ID, &id)
	}
	reason := de.err.Error()
	if len(reason) > maxInvalidReasonLen {
		reason = reason[:maxInvalidReasonLen] + "..."
	}
	return id, "ipc: invalid request: " + reason, true
}
