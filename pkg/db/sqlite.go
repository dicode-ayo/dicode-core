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
		// Ensure the file exists and is readable only by the owner. The 0600
		// perm on OpenFile only applies at creation time; Chmod also repairs
		// pre-existing databases that were created before this fix.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("create db file: %w", err)
		}
		if err := f.Chmod(0600); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("chmod db file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close db file: %w", err)
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
	// Add new columns to existing tables via the proper migration helper so
	// genuine failures are surfaced rather than silently swallowed.
	ctx := context.Background()
	for _, m := range []struct{ name, ddl string }{
		{"trigger_source", "TEXT NOT NULL DEFAULT ''"},
		{"return_value", "TEXT"},
		{"output_content_type", "TEXT NOT NULL DEFAULT ''"},
		{"output_content", "TEXT"},
		{"fail_reason", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumnIfMissing(ctx, s.db, "runs", m.name, m.ddl); err != nil {
			return fmt.Errorf("migrate runs.%s: %w", m.name, err)
		}
	}

	// Auth tables — sessions (browser sessions + trusted devices) and API keys.
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id          TEXT PRIMARY KEY,
			token_hash  TEXT NOT NULL UNIQUE,
			kind        TEXT NOT NULL CHECK(kind IN ('session','device')),
			label       TEXT NOT NULL DEFAULT '',
			ip          TEXT NOT NULL DEFAULT '',
			ua_family   TEXT,
			drift_reason TEXT NOT NULL DEFAULT '',
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
	if err != nil {
		return err
	}

	// Incremental migrations on `runs` (introduced for #233 — first usage of
	// ALTER TABLE in dicode's schema). Each entry is idempotent.
	runsMigrations := []struct {
		name string
		ddl  string
	}{
		{"input_storage_key", "TEXT"},
		{"input_size", "INTEGER"},
		{"input_stored_at", "INTEGER"},
		{"input_pinned", "INTEGER NOT NULL DEFAULT 0"},
		{"input_redacted_fields", "TEXT"},
		// Run grouping (#116) — free-text label set by the task itself via
		// dicode.set_group(). Column is `run_group` (not `group`) because the
		// latter is a SQL keyword and our migrate helper rejects quoted idents.
		{"run_group", "TEXT NOT NULL DEFAULT ''"},
		// kind discriminator for PipelineTask runs. 'task' (default) for
		// normal Task runs; 'pipeline' will be set on a PipelineTask's
		// parent run by the engine (upcoming PR). SQLite backfills existing
		// rows to 'task' at ALTER time.
		{"kind", "TEXT NOT NULL DEFAULT 'task'"},
		// Suspend/resume columns (#95). All nullable — set only when a run
		// suspends awaiting user input. resume_state/resume_form hold opaque
		// JSON blobs; resume_token is an unguessable resume handle; the two
		// timestamps are Unix milliseconds (resume_deadline is the TTL after
		// which a still-suspended run is cancelled).
		{"resume_state", "BLOB"},
		{"resume_form", "BLOB"},
		{"resume_token", "TEXT"},
		{"suspended_at", "INTEGER"},
		{"resume_deadline", "INTEGER"},
		// resume_params carries the ORIGINAL run's fire-time param overrides and
		// chain depth as a JSON envelope so the continuation resumes with the same
		// ctx.params and honors the same chain-depth ceiling. NULL when the
		// suspended run carried neither.
		{"resume_params", "BLOB"},
		// root_run_id (#569) — the topmost ancestor of a run's parent chain
		// (chain / pipeline stage / replay / suspend-resume continuation). A
		// parentless run is its own root. Denormalized at insert time
		// (Registry.StartRunWithID) so grouping a run's whole descendant tree
		// under one collapsible unit (ListRunGroup) is a single indexed WHERE
		// instead of a recursive walk. NULL only for rows written before this
		// migration, until backfillRootRunID runs.
		{"root_run_id", "TEXT"},
	}
	for _, m := range runsMigrations {
		if err := addColumnIfMissing(ctx, s.db, "runs", m.name, m.ddl); err != nil {
			return fmt.Errorf("migrate runs.%s: %w", m.name, err)
		}
	}
	if err := backfillRootRunID(ctx, s.db); err != nil {
		return fmt.Errorf("backfill root_run_id: %w", err)
	}

	// ua_family on trusted-device rows. Nullable on purpose: rows issued
	// before this migration have no recorded UA family and must not be treated
	// as a mismatch — see renewFromDevice for the NULL-is-not-drift handling.
	if err := addColumnIfMissing(ctx, s.db, "sessions", "ua_family", "TEXT"); err != nil {
		return fmt.Errorf("migrate sessions.ua_family: %w", err)
	}
	// drift_reason records the last observed device-binding drift ("ip", "ua",
	// "ip+ua") for warn mode so /security can surface it. Empty string means no
	// drift seen on this row.
	if err := addColumnIfMissing(ctx, s.db, "sessions", "drift_reason", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate sessions.drift_reason: %w", err)
	}

	// scopes (#567) — JSON-encoded runtime.MCPScope for ephemeral per-run MCP
	// API keys, restricting the MCP tool surface a key may call to the
	// owning task's own declared permissions.dicode. NULL/empty means
	// unscoped (full access) — the default, so every existing dashboard- or
	// CLI-managed key keeps working unchanged.
	if err := addColumnIfMissing(ctx, s.db, "api_keys", "scopes", "TEXT"); err != nil {
		return fmt.Errorf("migrate api_keys.scopes: %w", err)
	}

	// scs_sessions — SQLite-backed store for alexedwards/scs/v2 session
	// manager. Replaces the former in-memory sessionStore.
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS scs_sessions (
			token      TEXT PRIMARY KEY,
			data       BLOB NOT NULL,
			expires_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_scs_sessions_expires ON scs_sessions(expires_at);
	`)
	if err != nil {
		return fmt.Errorf("migrate scs_sessions: %w", err)
	}

	// Indexes supporting #116 run-grouping filters. CREATE INDEX IF NOT EXISTS
	// is idempotent so this is safe to run on every startup.
	_, err = s.db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_runs_parent_run_id ON runs(parent_run_id);
		CREATE INDEX IF NOT EXISTS idx_runs_task_run_group ON runs(task_id, run_group);
	`)
	if err != nil {
		return fmt.Errorf("migrate run-grouping indexes: %w", err)
	}

	// root_run_id index (#569) — ListRunGroup and GetRunGroupLogs both filter
	// on it; without an index they'd full-scan runs/run_logs as history grows.
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_runs_root_run_id ON runs(root_run_id);`)
	if err != nil {
		return fmt.Errorf("migrate root_run_id index: %w", err)
	}

	// Suspend/resume lookup indexes (#95). Without them the resume-token lookup
	// (GetRunByResumeToken) and the per-minute deadline sweep (SweepExpiredSuspensions)
	// full-scan `runs`, which grows unbounded with run history. The token index
	// is partial: only suspended/resumed rows carry a token, so it stays tiny
	// against a table that is overwhelmingly terminal runs.
	_, err = s.db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_runs_resume_token ON runs(resume_token) WHERE resume_token IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_runs_status_resume_deadline ON runs(status, resume_deadline);
	`)
	if err != nil {
		return fmt.Errorf("migrate resume indexes: %w", err)
	}

	// author_sessions — AI-first task authoring sessions (#288).
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS author_sessions (
			id           TEXT PRIMARY KEY,
			kind         TEXT NOT NULL CHECK(kind IN ('create','edit')),
			source       TEXT NOT NULL,
			task_id      TEXT NOT NULL DEFAULT '',
			sandbox_path TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL,
			last_turn_at INTEGER NOT NULL,
			closed_at    INTEGER,
			applied      INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		return fmt.Errorf("migrate author_sessions: %w", err)
	}
	// Enforce single-session-per-source at the DB level. Two concurrent
	// edit requests can race past the application-level GetOpenForSource
	// check (TOCTOU); this partial unique index makes the second INSERT
	// fail with a constraint violation.
	_, err = s.db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_author_sessions_open_source
		ON author_sessions(source) WHERE closed_at IS NULL;
	`)
	if err != nil {
		return fmt.Errorf("migrate author_sessions index: %w", err)
	}

	// approval_tokens — single-use approve-link tokens (#398). token_hash is
	// the SHA-256 of the raw token; the raw value is never stored.
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS approval_tokens (
			token_hash   TEXT PRIMARY KEY,
			task_id      TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			expires_at   INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_approval_tokens_expires ON approval_tokens(expires_at);
	`)
	if err != nil {
		return fmt.Errorf("migrate approval_tokens: %w", err)
	}
	// audit_log — structured audit log for security-sensitive operations
	// (#45). Appended by pkg/audit at the trigger-engine, IPC (run_task /
	// mcp.call), and webui denied-auth boundaries. params holds sanitized
	// JSON only — secret values are redacted before insert.
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_log (
			id          TEXT PRIMARY KEY,
			ts          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			event_type  TEXT NOT NULL DEFAULT '',
			actor_kind  TEXT NOT NULL DEFAULT '',
			actor_id    TEXT NOT NULL DEFAULT '',
			target_kind TEXT NOT NULL DEFAULT '',
			target_id   TEXT NOT NULL DEFAULT '',
			params      TEXT NOT NULL DEFAULT '',
			run_id      TEXT NOT NULL DEFAULT '',
			allowed     BOOLEAN NOT NULL,
			reason      TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON audit_log(ts DESC);
		CREATE INDEX IF NOT EXISTS idx_audit_log_actor_ts ON audit_log(actor_id, ts DESC);
	`)
	if err != nil {
		return fmt.Errorf("migrate audit_log: %w", err)
	}

	return nil
}

// backfillRootRunID fills root_run_id on rows written before the #569
// migration. It only ever touches rows where root_run_id IS NULL, so on every
// startup after the first it's a cheap no-op (new rows always insert with
// root_run_id set by Registry.StartRunWithID).
//
// Parentless rows get root_run_id = id directly. Child rows resolve by
// walking the parent_run_id chain one hop per pass — bounded to
// maxRootBackfillPasses so a pathological chain can't loop forever — until no
// more rows can be resolved. Any row still NULL afterwards (its parent chain
// bottoms out at a missing/deleted row, or the chain is deeper than the pass
// bound) falls back to root_run_id = id: it renders as its own top-level
// group rather than joining its ancestor's, which is the safe degradation.
func backfillRootRunID(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE runs SET root_run_id = id
		 WHERE root_run_id IS NULL AND (parent_run_id IS NULL OR parent_run_id = '')`,
	); err != nil {
		return fmt.Errorf("backfill root rows: %w", err)
	}
	const maxRootBackfillPasses = 100
	for i := 0; i < maxRootBackfillPasses; i++ {
		res, err := db.ExecContext(ctx,
			`UPDATE runs SET root_run_id = (
			     SELECT p.root_run_id FROM runs p WHERE p.id = runs.parent_run_id
			 )
			 WHERE root_run_id IS NULL
			   AND parent_run_id IS NOT NULL AND parent_run_id != ''
			   AND EXISTS (
			       SELECT 1 FROM runs p
			       WHERE p.id = runs.parent_run_id AND p.root_run_id IS NOT NULL
			   )`,
		)
		if err != nil {
			return fmt.Errorf("backfill child rows (pass %d): %w", i, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("backfill child rows (pass %d): rows affected: %w", i, err)
		}
		if n == 0 {
			break
		}
	}
	// Stragglers: orphaned parent_run_id (parent row missing/deleted) or a
	// chain deeper than maxRootBackfillPasses. Self-root them.
	if _, err := db.ExecContext(ctx, `UPDATE runs SET root_run_id = id WHERE root_run_id IS NULL`); err != nil {
		return fmt.Errorf("backfill straggler rows: %w", err)
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

func (s *SQLiteDB) ExecResult(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

func (t *sqliteTx) ExecResult(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
