package runtime

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dicode/dicode/pkg/runtime/envresolve"
)

// TestBridgeDeps_SetProtectedPaths pins the path-hygiene contract both
// runtimes relied on before the #388 dedup: empty entries are dropped and
// every kept path is filepath.Clean'ed so "../" segments and redundant
// separators resolve before they reach the enforcement layer (Deno
// --deny-write flags / the Python guard policy deny list).
func TestBridgeDeps_SetProtectedPaths(t *testing.T) {
	var d BridgeDeps
	d.SetProtectedPaths([]string{
		"",                           // dropped
		"/data//dicode.lock",         // redundant separator collapsed
		"/data/tasks/../dicode.yaml", // ../ resolved
		"/already/clean",
	})

	want := []string{
		filepath.Clean("/data//dicode.lock"),
		filepath.Clean("/data/tasks/../dicode.yaml"),
		"/already/clean",
	}
	if !reflect.DeepEqual(d.ProtectedPaths, want) {
		t.Errorf("SetProtectedPaths = %v, want %v", d.ProtectedPaths, want)
	}
}

// TestBridgeDeps_SetProtectedPaths_AllEmpty verifies an all-empty input
// yields an empty (non-nil is fine) deny list rather than a list of "".
func TestBridgeDeps_SetProtectedPaths_AllEmpty(t *testing.T) {
	var d BridgeDeps
	d.SetProtectedPaths([]string{"", ""})
	if len(d.ProtectedPaths) != 0 {
		t.Errorf("SetProtectedPaths([\"\", \"\"]) = %v, want empty", d.ProtectedPaths)
	}
}

// TestBridgeDeps_SetSecretOutputChannel_Clear covers a setter whose stored
// value is consumed indirectly (no getter): the field must be exactly what
// was passed, including nil to clear.
func TestBridgeDeps_SetSecretOutputChannel_Clear(t *testing.T) {
	var d BridgeDeps
	ch := make(chan map[string]string, 1)
	d.SetSecretOutputChannel(ch)
	if d.SecretOutputCh != ch {
		t.Fatal("SetSecretOutputChannel did not store the channel")
	}
	// The trigger engine clears the channel after each provider run.
	d.SetSecretOutputChannel(nil)
	if d.SecretOutputCh != nil {
		t.Fatal("SetSecretOutputChannel(nil) did not clear the channel")
	}
}

// TestBridgeDeps_SetEnvResolver pins the shared-resolver setter: the exact
// instance must be stored (LiveResolver's precedence depends on identity),
// and nil must clear it.
func TestBridgeDeps_SetEnvResolver(t *testing.T) {
	var d BridgeDeps
	r := envresolve.New(nil, nil, nil)
	d.SetEnvResolver(r)
	if d.SharedResolver != r {
		t.Fatal("SetEnvResolver did not store the resolver")
	}
	d.SetEnvResolver(nil)
	if d.SharedResolver != nil {
		t.Fatal("SetEnvResolver(nil) did not clear the resolver")
	}
}
