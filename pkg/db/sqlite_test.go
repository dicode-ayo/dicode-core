package db

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) DB {
	t.Helper()
	db, err := openSQLite(":memory:")
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSQLiteDB_PingClose(t *testing.T) {
	db := newTestDB(t)
	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestSQLiteDB_ExecQuery(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := db.Exec(ctx, `INSERT INTO kv (key, value) VALUES (?, ?)`, "hello", "world"); err != nil {
		t.Fatalf("exec insert: %v", err)
	}

	var got string
	err := db.Query(ctx, `SELECT value FROM kv WHERE key = ?`, []any{"hello"}, func(rows Scanner) error {
		for rows.Next() {
			return rows.Scan(&got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != "world" {
		t.Fatalf("got %q, want %q", got, "world")
	}
}

func TestSQLiteDB_Tx_Commit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	err := db.Tx(ctx, func(tx DB) error {
		return tx.Exec(ctx, `INSERT INTO kv (key, value) VALUES (?, ?)`, "k", "v")
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	var count int
	_ = db.Query(ctx, `SELECT COUNT(*) FROM kv WHERE key = ?`, []any{"k"}, func(rows Scanner) error {
		rows.Next()
		return rows.Scan(&count)
	})
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

func TestSQLiteDB_Tx_Rollback(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	_ = db.Tx(ctx, func(tx DB) error {
		_ = tx.Exec(ctx, `INSERT INTO kv (key, value) VALUES (?, ?)`, "k2", "v2")
		return errRollback
	})

	var count int
	_ = db.Query(ctx, `SELECT COUNT(*) FROM kv WHERE key = ?`, []any{"k2"}, func(rows Scanner) error {
		rows.Next()
		return rows.Scan(&count)
	})
	if count != 0 {
		t.Fatalf("expected rollback, got %d rows", count)
	}
}

var errRollback = &UnsupportedBackendError{Type: "rollback-test"}

func TestSQLiteDB_RunsHasInputColumns(t *testing.T) {
	d := newTestDB(t).(*SQLiteDB)
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `PRAGMA table_info(runs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := map[string]string{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = ctype
	}
	wantCols := []string{
		"input_storage_key",
		"input_size",
		"input_stored_at",
		"input_pinned",
		"input_redacted_fields",
	}
	for _, want := range wantCols {
		if _, ok := cols[want]; !ok {
			t.Errorf("runs missing column %q", want)
		}
	}
}

func TestSQLiteDB_Migrate_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.db")

	d1, err := openSQLite(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	d1.Close()

	d2, err := openSQLite(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	d2.Close()
}

func TestSQLiteDB_Schema(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tables := []string{"runs", "run_logs", "kv", "secrets"}
	for _, tbl := range tables {
		var name string
		_ = db.Query(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`,
			[]any{tbl},
			func(rows Scanner) error {
				if rows.Next() {
					return rows.Scan(&name)
				}
				return nil
			},
		)
		if name != tbl {
			t.Errorf("table %q not found", tbl)
		}
	}
}
