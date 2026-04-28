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
	db, err := sql.Open("sqlite", path+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Serialize all DB access through a single connection. SQLite only allows
	// one writer at a time regardless of mode; multiple connections within the
	// same process race on writes and produce SQLITE_BUSY. A single connection
	// eliminates intra-process lock contention entirely.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	// Apply pragmas via exec — modernc.org/sqlite does not honour DSN-style
	// _pragma parameters reliably. WAL allows readers to proceed concurrently
	// with the single writer. busy_timeout is a belt-and-suspenders safeguard
	// for any external process (e.g. sqlite3 CLI) that may also open the file.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, fmt.Errorf("sqlite pragma %q: %w", pragma, err)
		}
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

		CREATE TABLE IF NOT EXISTS cron_jobs (
			task_id     TEXT PRIMARY KEY,
			cron_expr   TEXT NOT NULL,
			last_run_at INTEGER,
			next_run_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	// Add new columns to existing tables (errors suppressed — expected on re-run).
	for _, stmt := range []string{
		`ALTER TABLE runs ADD COLUMN trigger_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN return_value TEXT`,
		`ALTER TABLE runs ADD COLUMN output_content_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN output_content TEXT`,
		`ALTER TABLE runs ADD COLUMN fail_reason TEXT NOT NULL DEFAULT ''`,
	} {
		_, _ = s.db.Exec(stmt)
	}

	// Auth tables — sessions (browser sessions + trusted devices) and API keys.
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id          TEXT PRIMARY KEY,
			token_hash  TEXT NOT NULL UNIQUE,
			kind        TEXT NOT NULL CHECK(kind IN ('session','device')),
			label       TEXT NOT NULL DEFAULT '',
			ip          TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			last_seen   INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

		CREATE TABLE IF NOT EXISTS api_keys (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			key_hash   TEXT NOT NULL UNIQUE,
			prefix     TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_used  INTEGER,
			expires_at INTEGER
		);
	`)
	return err
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
