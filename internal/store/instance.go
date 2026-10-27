package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// A database is marked demo when a demo instance (COACHD_DEMO=true) creates
// it. serve refuses a demo database outside demo mode and any other database
// in demo mode, and restore refuses to swap one kind for the other, so
// synthetic data never mixes with the runner's Garmin data. The mark lives in
// the database, so it travels with backups.

const demoMode = "demo"

// MarkDemo marks the database as holding demo data. It is idempotent.
func (db *DB) MarkDemo(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO instance (key, value) VALUES ('mode', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, demoMode); err != nil {
		return fmt.Errorf("store: mark demo: %w", err)
	}
	return nil
}

// Demo reports whether the database is marked demo.
func (db *DB) Demo(ctx context.Context) (bool, error) {
	demo, err := readDemo(ctx, db.sql)
	if err != nil {
		return false, fmt.Errorf("store: demo mark: %w", err)
	}
	return demo, nil
}

// ReadDemo reports whether the coachd database at path is marked demo,
// without migrating it: a database from before the mark existed is not demo.
// A file without the SQLite header is an error, read unopened, as in
// ReadCarryForward. Reading may checkpoint the database's -wal.
func ReadDemo(ctx context.Context, path string) (bool, error) {
	if err := sqliteHeader(path); err != nil {
		return false, fmt.Errorf("store: read demo mark: %w", err)
	}
	sdb, err := openPlain(path)
	if err != nil {
		return false, fmt.Errorf("store: read demo mark: %w", err)
	}
	defer sdb.Close()
	demo, err := readDemo(ctx, sdb)
	if err != nil {
		return false, fmt.Errorf("store: read demo mark: %w", err)
	}
	return demo, nil
}

// readDemo reads the mark; a schema without the instance table has none.
func readDemo(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bool, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM instance WHERE key='mode'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil && strings.Contains(err.Error(), "no such table: instance"):
		return false, nil
	case err != nil:
		return false, err
	}
	return v == demoMode, nil
}
