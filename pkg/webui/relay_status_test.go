package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dicode/dicode/pkg/relay"
	"go.uber.org/zap"
)

func TestAPIRelayStatus_NoClient_ReturnsDisabled(t *testing.T) {
	// No SetRelayClient call — the server has relayClient == nil.
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/relay/status", nil)
	s.apiRelayStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["enabled"] != false {
		t.Errorf("enabled = %v; want false when no relay client", got["enabled"])
	}
}

func TestAPIRelayStatus_WithClient_SerializesStatus(t *testing.T) {
	client := relay.NewClient("ws://test.example", nil, 0, nil, zap.NewNop())

	s := &Server{}
	s.SetRelayClient(client)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/relay/status", nil)
	s.apiRelayStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var got relay.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled {
		t.Error("enabled should be true when a client is registered")
	}
	if got.RemoteURL != "ws://test.example" {
		t.Errorf("remote_url = %q; want ws://test.example", got.RemoteURL)
	}
	if got.Connected {
		t.Error("connected should be false before handshake")
	}
	_ = context.TODO() // silence unused import on some toolchains
}
