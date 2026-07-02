package db

import (
	"errors"
	"testing"
)

// TestOpen_Postgres_ReturnsErrorNotPanic guards against a regression where
// Open panicked instead of returning an error for a backend type that is
// recognized but not yet implemented. A `type: postgres` typo/misconfig in
// dicode.yaml must fail daemon startup cleanly (see pkg/daemon.run, which
// wraps and returns this error) rather than crash the process.
func TestOpen_Postgres_ReturnsErrorNotPanic(t *testing.T) {
	d, err := Open(Config{Type: "postgres", URLEnv: "DATABASE_URL"})
	if d != nil {
		t.Errorf("expected nil DB, got %v", d)
	}
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var niErr *NotImplementedError
	if !errors.As(err, &niErr) {
		t.Fatalf("expected *NotImplementedError, got %T: %v", err, err)
	}
	if niErr.Type != "postgres" {
		t.Errorf("NotImplementedError.Type = %q, want %q", niErr.Type, "postgres")
	}
}

func TestOpen_MySQL_ReturnsErrorNotPanic(t *testing.T) {
	d, err := Open(Config{Type: "mysql", URLEnv: "DATABASE_URL"})
	if d != nil {
		t.Errorf("expected nil DB, got %v", d)
	}
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var niErr *NotImplementedError
	if !errors.As(err, &niErr) {
		t.Fatalf("expected *NotImplementedError, got %T: %v", err, err)
	}
	if niErr.Type != "mysql" {
		t.Errorf("NotImplementedError.Type = %q, want %q", niErr.Type, "mysql")
	}
}

func TestOpen_UnknownType_ReturnsUnsupportedBackendError(t *testing.T) {
	d, err := Open(Config{Type: "postgress"}) // typo of a typo — still unrecognized
	if d != nil {
		t.Errorf("expected nil DB, got %v", d)
	}
	var ubErr *UnsupportedBackendError
	if !errors.As(err, &ubErr) {
		t.Fatalf("expected *UnsupportedBackendError, got %T: %v", err, err)
	}
}

func TestOpen_SQLite_DefaultAndExplicit(t *testing.T) {
	for _, typ := range []string{"", "sqlite"} {
		d, err := Open(Config{Type: typ, Path: ":memory:"})
		if err != nil {
			t.Fatalf("Open(Type:%q): %v", typ, err)
		}
		defer d.Close()
	}
}
