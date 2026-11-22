package ipc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func writeRawFrame(t *testing.T, conn net.Conn, body []byte) {
	t.Helper()
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := conn.Write(append(hdr[:], body...)); err != nil {
		t.Fatalf("write raw frame: %v", err)
	}
}

func frame(body string) []byte {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	return append(hdr[:], body...)
}

func expectClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var m map[string]any
	err := readMsg(conn, &m)
	if err == nil {
		t.Fatalf("expected connection to close, got message %v", m)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("connection stayed open after a framing error")
	}
}

func TestReadMsg_ErrorClassification(t *testing.T) {
	tooBig := make([]byte, 4)
	binary.LittleEndian.PutUint32(tooBig, maxMessageSize+1)

	cases := []struct {
		name   string
		in     []byte
		decode bool
		id     string
	}{
		{"empty stream", nil, false, ""},
		{"short header", []byte{1, 0}, false, ""},
		{"zero length", []byte{0, 0, 0, 0}, false, ""},
		{"oversize", tooBig, false, ""},
		{"short body", frame(`{"id":"1"}`)[:8], false, ""},
		{"wrong field type", frame(`{"id":"7","method":"kv.get","key":123}`), true, "7"},
		{"numeric id", frame(`{"id":7,"method":"kv.get"}`), true, ""},
		{"no id", frame(`{"method":"kv.get","key":123}`), true, ""},
		{"not json", frame(`not json`), true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req Request
			err := readMsg(bytes.NewReader(tc.in), &req)
			if err == nil {
				t.Fatal("expected error")
			}
			id, msg, ok := invalidRequest(err)
			if ok != tc.decode {
				t.Fatalf("decode error = %v, want %v (err: %v)", ok, tc.decode, err)
			}
			if id != tc.id {
				t.Errorf("id = %q, want %q", id, tc.id)
			}
			if ok && !strings.HasPrefix(msg, "ipc: invalid request: ") {
				t.Errorf("msg = %q", msg)
			}
		})
	}
}

func TestReadMsg_DecodeErrorLeavesStreamInSync(t *testing.T) {
	r := bytes.NewReader(append(frame(`{"id":"1","key":123}`), frame(`{"id":"2","key":"k"}`)...))
	var req Request
	if _, _, ok := invalidRequest(readMsg(r, &req)); !ok {
		t.Fatal("first frame should be a decode error")
	}
	req = Request{}
	if err := readMsg(r, &req); err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if req.ID != "2" || req.Key != "k" {
		t.Errorf("second frame decoded as %+v", req)
	}
	if err := readMsg(r, &req); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestInvalidRequest_TruncatesReason(t *testing.T) {
	_, msg, ok := invalidRequest(&decodeError{raw: []byte(`{"id":"1"}`), err: errors.New(strings.Repeat("x", 10000))})
	if !ok {
		t.Fatal("expected decode error")
	}
	if len(msg) > 300 {
		t.Errorf("reason not truncated: %d bytes", len(msg))
	}
}

func TestServer_BadlyTypedRequest_RepliesAndKeepsServing(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	writeRawFrame(t, conn, []byte(`{"id":"1","method":"kv.get","key":123}`))
	resp := recvMsg(t, conn)
	if resp["id"] != "1" {
		t.Fatalf("expected reply to id 1, got %v", resp)
	}
	if errMsg, _ := resp["error"].(string); !strings.HasPrefix(errMsg, "ipc: invalid request: ") {
		t.Fatalf("expected invalid request error, got %v", resp)
	}

	sendMsg(t, conn, map[string]any{"method": "kv.set", "key": "k", "value": "v"})
	sendMsg(t, conn, map[string]any{"id": "3", "method": "kv.get", "key": "k"})
	if resp := recvMsg(t, conn); resp["id"] != "3" || resp["result"] != "v" {
		t.Fatalf("kv.get after bad request: %v", resp)
	}
}

func TestServer_BadlyTypedRequestWithoutID_DroppedNotDispatched(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	// Decodes far enough to fill method/key/value before the bad field.
	writeRawFrame(t, conn, []byte(`{"method":"kv.set","key":"k","value":"v","level":5}`))
	writeRawFrame(t, conn, []byte(`{"id":9,"method":"kv.set","key":"k","value":"v"}`))

	sendMsg(t, conn, map[string]any{"id": "2", "method": "kv.get", "key": "k"})
	resp := recvMsg(t, conn)
	if resp["id"] != "2" {
		t.Fatalf("expected reply to id 2, got %v", resp)
	}
	if resp["result"] != nil {
		t.Fatalf("partly decoded kv.set was dispatched: %v", resp)
	}
}

func TestServer_FramingError_ClosesConnection(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil)

	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, conn)
}

func TestControl_BadlyTypedRequest_RepliesAndKeepsServing(t *testing.T) {
	conn, cleanup := controlTestEnv(t, MetricsProvider{})
	defer cleanup()

	writeRawFrame(t, conn, []byte(`{"id":"1","method":"cli.metrics","key":123}`))
	resp := recvMsg(t, conn)
	if resp["id"] != "1" {
		t.Fatalf("expected reply to id 1, got %v", resp)
	}
	if errMsg, _ := resp["error"].(string); !strings.HasPrefix(errMsg, "ipc: invalid request: ") {
		t.Fatalf("expected invalid request error, got %v", resp)
	}

	sendMsg(t, conn, map[string]any{"id": "2", "method": "cli.metrics"})
	if resp := recvMsg(t, conn); resp["id"] != "2" || resp["error"] != nil {
		t.Fatalf("cli.metrics after bad request: %v", resp)
	}
}

func TestControl_BadlyTypedRequestWithoutID_Dropped(t *testing.T) {
	conn, cleanup := controlTestEnv(t, MetricsProvider{})
	defer cleanup()

	writeRawFrame(t, conn, []byte(`{"method":"cli.metrics","key":123}`))
	writeRawFrame(t, conn, []byte(`{"id":1,"method":"cli.metrics","key":123}`))

	sendMsg(t, conn, map[string]any{"id": "2", "method": "cli.metrics"})
	if resp := recvMsg(t, conn); resp["id"] != "2" || resp["error"] != nil {
		t.Fatalf("expected cli.metrics reply to id 2, got %v", resp)
	}
}

func TestControl_FramingError_ClosesConnection(t *testing.T) {
	conn, cleanup := controlTestEnv(t, MetricsProvider{})
	defer cleanup()

	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, conn)
}
