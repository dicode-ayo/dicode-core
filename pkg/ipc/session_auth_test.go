package ipc

import (
	"context"
	"testing"
)

func TestSessionAuth(t *testing.T) {
	// Absent ⇒ false ⇒ fail-closed.
	if SessionAuthed(context.Background()) {
		t.Error("bare context must not be session-authed")
	}
	// Round-trips through WithSessionAuth.
	ctx := WithSessionAuth(context.Background())
	if !SessionAuthed(ctx) {
		t.Error("WithSessionAuth context must be session-authed")
	}
	// A derived child context still carries the flag.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if !SessionAuthed(child) {
		t.Error("child context must inherit the session-auth flag")
	}
}
