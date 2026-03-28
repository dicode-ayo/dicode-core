package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// SQLiteDB implements the DB interface using modernc.org/sqlite (pure Go, no CGo).
type SQLiteDB struct {
	db *sql.DB
}

func openSQLite(path string) (DB, error) {
	if path == "" {
		path = ":memory:"
	}
	// Expand ~ and ensure parent directory exists before SQLite tries to open the file.
	if len(path) >= 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		path = home + path[1:]
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// In-memory databases are per-connection: every connection gets its own
	// empty DB. A single connection prevents the silent isolation bug where
	// two goroutines each see a different empty database.
	// File-based DBs use WAL mode, which allows concurrent readers alongside
	// one writer — no connection limit needed there.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &SQLiteDB{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *SQLiteDB) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS runs (
			id           TEXT PRIMARY KEY,
			task_id      TEXT NOT NULL,
			status       TEXT NOT NULL DEFAULT 'running',
			started_at   INTEGER NOT NULL,
			finished_at  INTEGER,
			parent_run_id TEXT
		);

		CREATE TABLE IF NOT EXISTS run_logs (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id  TEXT NOT NULL,
			ts      INTEGER NOT NULL,
			level   TEXT NOT NULL,
			message TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_run_logs_run_id ON run_logs(run_id);
		CREATE INDEX IF NOT EXISTS idx_runs_task_id ON runs(task_id);

		CREATE TABLE IF NOT EXISTS kv (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS secrets (
			key        TEXT PRIMARY KEY,
			ciphertext BLOB NOT NULL,
			nonce      BLOB NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	// Add new columns to existing tables (errors suppressed — expected on re-run).
	for _, stmt := range []string{
		`ALTER TABLE runs ADD COLUMN trigger_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN return_value TEXT`,
	} {
		_, _ = s.db.Exec(stmt)
	}
	return nil
}

func (s *SQLiteDB) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *SQLiteDB) Close() error {
	return s.db.Close()
}

func (s *SQLiteDB) Exec(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *SQLiteDB) Query(ctx context.Context, query string, args []any, scan func(rows Scanner) error) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return scan(rows)
}

func (s *SQLiteDB) Tx(ctx context.Context, fn func(tx DB) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(&sqliteTx{tx: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// sqliteTx wraps sql.Tx to implement DB inside a transaction.
type sqliteTx struct {
	tx *sql.Tx
}

func (t *sqliteTx) Ping(_ context.Context) error { return nil }
func (t *sqliteTx) Close() error                 { return nil }

func (t *sqliteTx) Exec(ctx context.Context, query string, args ...any) error {
	_, err := t.tx.ExecContext(ctx, query, args...)
	return err
}

func (t *sqliteTx) Query(ctx context.Context, query string, args []any, scan func(rows Scanner) error) error {
	rows, err := t.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return scan(rows)
}

func (t *sqliteTx) Tx(ctx context.Context, fn func(tx DB) error) error {
	// SQLite doesn't support nested transactions; reuse the same tx.
	return fn(t)
}
