package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

// identRe matches valid SQL identifiers: ASCII letter or underscore as first
// character, followed by zero or more ASCII letters, digits, or underscores.
// Used to validate table and column names before interpolating them into SQL
// statements. Defense in depth — current callers are all hardcoded constants.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// addColumnIfMissing adds a column to a table if it doesn't already exist.
// Idempotent: calling it again with the same arguments is a no-op.
//
// This is the building block for incremental schema migrations layered on top
// of the existing CREATE TABLE IF NOT EXISTS statements in migrate(). When a
// future migration needs richer semantics (renames, backfills), a real
// versioned migration framework can be introduced; for now this helper keeps
// the diff small and the migration story honest.
//
// Operates on *sql.DB directly (rather than the DB interface) because it's
// an internal helper called from migrate() which already holds the underlying
// connection. Callers from outside pkg/db do not exist by design.
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, ddl string) error {
	if !identRe.MatchString(table) {
		return fmt.Errorf("addColumnIfMissing: invalid table identifier %q", table)
	}
	if !identRe.MatchString(column) {
		return fmt.Errorf("addColumnIfMissing: invalid column identifier %q", column)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("table_info(%s): %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("alter %s add %s: %w", table, column, err)
	}
	return nil
}
