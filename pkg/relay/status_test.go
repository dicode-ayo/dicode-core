package relay

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	return NewClient("ws://test.example", nil, 0, nil, zap.NewNop())
}

func TestClient_Status_ZeroValue(t *testing.T) {
	c := newTestClient(t)
	s := c.Status()
	if !s.Enabled {
		t.Error("Status.Enabled should be true for any constructed Client")
	}
	if s.Connected {
		t.Error("Status.Connected should be false before handshake")
	}
	if s.RemoteURL != "ws://test.example" {
		t.Errorf("Status.RemoteURL = %q; want ws://test.example", s.RemoteURL)
	}
	if s.ReconnectAttempts != 0 {
		t.Errorf("Status.ReconnectAttempts = %d; want 0", s.ReconnectAttempts)
	}
}

func TestClient_MarkConnected_UpdatesStatus(t *testing.T) {
	c := newTestClient(t)
	before := time.Now()
	c.markConnected()
	s := c.Status()
	if !s.Connected {
		t.Error("Connected should be true after markConnected")
	}
	if s.Since.Before(before) {
		t.Errorf("Since = %v; should be >= %v", s.Since, before)
	}
	if s.LastError != "" {
		t.Errorf("LastError = %q; should be cleared on connect", s.LastError)
	}
}

func TestClient_MarkDisconnected_SetsError(t *testing.T) {
	c := newTestClient(t)
	c.markConnected() // start from a known-good state
	want := errors.New("boom")
	c.markDisconnected(want)

	s := c.Status()
	if s.Connected {
		t.Error("Connected should be false after markDisconnected")
	}
	if s.LastError != want.Error() {
		t.Errorf("LastError = %q; want %q", s.LastError, want.Error())
	}
	if s.ReconnectAttempts != 1 {
		t.Errorf("ReconnectAttempts = %d; want 1", s.ReconnectAttempts)
	}
}

func TestClient_MarkDisconnected_Counts(t *testing.T) {
	c := newTestClient(t)
	for i := 0; i < 3; i++ {
		c.markDisconnected(errors.New("retry"))
	}
	s := c.Status()
	if s.ReconnectAttempts != 3 {
		t.Errorf("ReconnectAttempts = %d; want 3", s.ReconnectAttempts)
	}
}

func TestClient_MarkConnected_ResetsReconnectCount(t *testing.T) {
	c := newTestClient(t)
	c.markDisconnected(errors.New("one"))
	c.markDisconnected(errors.New("two"))
	if n := c.Status().ReconnectAttempts; n != 2 {
		t.Fatalf("precondition: ReconnectAttempts = %d; want 2", n)
	}
	c.markConnected()
	if n := c.Status().ReconnectAttempts; n != 0 {
		t.Errorf("ReconnectAttempts after connect = %d; want 0 (counter should reset)", n)
	}
}
