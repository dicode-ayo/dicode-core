// Package db provides a database abstraction over SQLite (free/default)
// and external databases such as PostgreSQL and MySQL (paid plans).
//
// All subsystems (registry, secrets store, run log) use this package
// rather than touching a database driver directly. Swapping the backend
// is a config change — no code changes required.
//
// Supported backends:
//   - sqlite  — embedded, zero-ops, single-machine (default, free)
//   - postgres — external, HA, multi-instance (paid)
//   - mysql    — external, HA, multi-instance (paid)
package db

import "context"

// DB is the top-level database handle. Concrete implementations are
// returned by Open().
type DB interface {
	// Ping verifies the connection is alive.
	Ping(ctx context.Context) error

	// Close releases all resources.
	Close() error

	// Exec executes a statement that returns no rows.
	Exec(ctx context.Context, query string, args ...any) error

	// Query executes a query and returns rows via the callback.
	Query(ctx context.Context, query string, args []any, scan func(rows Scanner) error) error

	// Tx runs fn inside a transaction. Rolls back on error or panic.
	Tx(ctx context.Context, fn func(tx DB) error) error
}

// Scanner matches *sql.Rows.Scan for use in Query callbacks.
type Scanner interface {
	Scan(dest ...any) error
	Next() bool
}

// Config mirrors config.DatabaseConfig to avoid circular imports.
type Config struct {
	Type   string // "sqlite" | "postgres" | "mysql"
	Path   string // sqlite path
	URLEnv string // env var holding DSN for postgres/mysql
}

// Open returns a DB handle for the configured backend.
// Returns an error if the backend type is unknown or connection fails.
func Open(cfg Config) (DB, error) {
	switch cfg.Type {
	case "sqlite", "":
		return openSQLite(cfg.Path)
	case "postgres":
		return openPostgres(cfg.URLEnv)
	case "mysql":
		return openMySQL(cfg.URLEnv)
	default:
		return nil, &UnsupportedBackendError{Type: cfg.Type}
	}
}

type UnsupportedBackendError struct{ Type string }

func (e *UnsupportedBackendError) Error() string {
	return "unsupported database type: " + e.Type + " (supported: sqlite, postgres, mysql)"
}

// Stub functions — implemented in sqlite.go, postgres.go, mysql.go
func openSQLite(_ string) (DB, error)  { panic("not yet implemented") }
func openPostgres(_ string) (DB, error) { panic("not yet implemented") }
func openMySQL(_ string) (DB, error)    { panic("not yet implemented") }
