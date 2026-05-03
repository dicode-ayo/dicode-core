package webui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIRelayStatus_Disabled(t *testing.T) {
	// No kv row → enabled:false
	srv, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/relay/status", nil)
	w := httptest.NewRecorder()
	srv.apiRelayStatus(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Errorf("expected enabled:false, got %s", w.Body.String())
	}
}

func TestAPIRelayStatus_FromKv(t *testing.T) {
	srv, _ := newTestServer(t)
	statusJSON := `{"connected":true,"hook_base_url":"https://relay.example/u/abc","reconnect_attempts":0}`
	if err := srv.db.Exec(context.Background(),
		`INSERT INTO kv (key, value) VALUES (?, ?)`,
		"buildin/relay-client:status", statusJSON,
	); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/relay/status", nil)
	w := httptest.NewRecorder()
	srv.apiRelayStatus(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["connected"] != true {
		t.Errorf("expected connected:true, got %v", got)
	}
	if got["hook_base_url"] != "https://relay.example/u/abc" {
		t.Errorf("expected hook URL, got %v", got)
	}
}
