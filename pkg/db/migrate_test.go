package db

import (
	"context"
	"testing"
)

func TestAddColumnIfMissing_AddsAndIsIdempotent(t *testing.T) {
	d := newTestDB(t).(*SQLiteDB)
	ctx := context.Background()

	if err := d.db.QueryRowContext(ctx, `SELECT 1`).Scan(new(int)); err != nil {
		// just verifies the test DB is usable; ignore errors here
	}

	if _, err := d.db.ExecContext(ctx, `CREATE TABLE foo (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := addColumnIfMissing(ctx, d.db, "foo", "bar", "TEXT"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	// Idempotent — calling again must not error.
	if err := addColumnIfMissing(ctx, d.db, "foo", "bar", "TEXT"); err != nil {
		t.Fatalf("second add (should be no-op): %v", err)
	}

	rows, err := d.db.QueryContext(ctx, `PRAGMA table_info(foo)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	want := []string{"id", "bar"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("cols = %v, want %v", got, want)
	}
}

func TestAddColumnIfMissing_AddsWithDefault(t *testing.T) {
	d := newTestDB(t).(*SQLiteDB)
	ctx := context.Background()
	if _, err := d.db.ExecContext(ctx, `CREATE TABLE bar (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO bar (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if err := addColumnIfMissing(ctx, d.db, "bar", "flag", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		t.Fatalf("add: %v", err)
	}
	var flag int
	if err := d.db.QueryRowContext(ctx, `SELECT flag FROM bar WHERE id = 1`).Scan(&flag); err != nil {
		t.Fatal(err)
	}
	if flag != 0 {
		t.Errorf("default not applied; flag = %d", flag)
	}
}
