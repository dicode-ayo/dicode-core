package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestClient wires a Client to an httptest server. The client's baseURL is
// the server's URL (the handler also handles the /mcp path the client POSTs to).
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	return New(port)
}

func TestListTools_Success(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/mcp" {
			t.Errorf("path = %q, want /mcp", r.URL.Path)
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Method != "tools/list" {
			t.Errorf("method = %q, want tools/list", req.Method)
		}
		_, _ = w.Write([]byte(`{
			"jsonrpc":"2.0","id":1,
			"result":{"tools":[
				{"name":"add","description":"adds two ints"},
				{"name":"echo","inputSchema":{"type":"object"}}
			]}
		}`))
	})

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools error: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(tools))
	}
	if tools[0].Name != "add" || tools[0].Description != "adds two ints" {
		t.Errorf("tool[0] = %+v, unexpected", tools[0])
	}
	if tools[1].Name != "echo" {
		t.Errorf("tool[1].Name = %q, want echo", tools[1].Name)
	}
	if len(tools[1].InputSchema) == 0 {
		t.Errorf("tool[1].InputSchema empty; want raw JSON passed through")
	}
}

func TestListTools_RPCError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	})

	_, err := client.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "-32601") || !strings.Contains(err.Error(), "method not found") {
		t.Errorf("error = %v; want to contain both the code and message", err)
	}
}

func TestListTools_HTTPError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	})

	_, err := client.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Plain "internal\n" is not valid JSON → decode error surfaces.
	if !strings.Contains(err.Error(), "decode mcp response") {
		t.Errorf("error = %v; want decode-response error on non-JSON body", err)
	}
}

func TestListTools_Malformed(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	})

	_, err := client.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "decode mcp response") {
		t.Errorf("error = %v; want decode error", err)
	}
}

func TestCall_Success(t *testing.T) {
	var gotName string
	var gotArgs map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Method != "tools/call" {
			t.Errorf("method = %q, want tools/call", req.Method)
		}
		// Params shape is a generic map in the request JSON.
		params, _ := req.Params.(map[string]any)
		gotName, _ = params["name"].(string)
		gotArgs, _ = params["arguments"].(map[string]any)

		_, _ = w.Write([]byte(`{
			"jsonrpc":"2.0","id":1,
			"result":{"content":[{"type":"text","text":"sum=7"}],"isError":false}
		}`))
	})

	got, err := client.Call(context.Background(), "add", map[string]any{"a": 3, "b": 4})
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if gotName != "add" {
		t.Errorf("server saw name=%q, want add", gotName)
	}
	if fmt.Sprint(gotArgs["a"]) != "3" || fmt.Sprint(gotArgs["b"]) != "4" {
		t.Errorf("server saw args=%+v, want a=3 b=4", gotArgs)
	}
	// Result passed through as a generic value (expected to be a map with content/isError).
	result, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got %T, want map[string]any", got)
	}
	if _, ok := result["content"]; !ok {
		t.Errorf("result missing `content` key: %+v", result)
	}
}

func TestCall_RPCError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params"}}`))
	})

	_, err := client.Call(context.Background(), "add", map[string]any{"a": "not-a-number"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "-32602") || !strings.Contains(err.Error(), "invalid params") {
		t.Errorf("error = %v; want code + message propagated", err)
	}
}

func TestCall_ConnectionRefused(t *testing.T) {
	// Bind an httptest server to grab an ephemeral port, then close it so we
	// point Client at a guaranteed-closed local port. More robust than
	// hardcoding port 1 (tcpmux), which can return EACCES/EPERM on restricted
	// CI sandboxes instead of the ECONNREFUSED we want.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	srv.Close()

	client := New(port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = client.ListTools(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "mcp request") {
		t.Errorf("error = %v; want wrapped 'mcp request' error", err)
	}
}
