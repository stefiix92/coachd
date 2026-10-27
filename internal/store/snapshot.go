package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// VerifySnapshot errors: a damaged or non-SQLite file, and an SQLite file
// that coachd did not write.
var (
	ErrCorruptSnapshot = errors.New("store: snapshot failed integrity check")
	ErrNotCoachdDB     = errors.New("store: not a coachd database")
)

// SnapshotInfo describes a verified snapshot.
type SnapshotInfo struct {
	SchemaVersion string // newest applied migration, e.g. "0008_deliveries"
	Demo          bool   // the snapshot is of a demo database (MarkDemo)
}

// SnapshotTo writes a consistent copy of the database to path (VACUUM INTO).
// Concurrent writers keep going; the copy holds the last commit before it
// started. It fails with fs.ErrExist if path exists, even as an empty file.
func (db *DB) SnapshotTo(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("store: snapshot: %w", fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: snapshot: %w", err)
	}
	if _, err := db.sql.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("store: snapshot: %w", err)
	}
	return nil
}

// fileURI returns a file: URI for path with the given query. The path is made
// absolute first: a relative one would render as file://<segment>/..., which
// SQLite rejects as a URI authority.
func fileURI(path, rawQuery string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: rawQuery}
	return u.String(), nil
}

// VerifySnapshot checks that path is an intact coachd database and returns
// its schema version. It opens the file read-only and immutable, so it never
// writes to it or creates -wal or -shm files next to it.
func VerifySnapshot(ctx context.Context, path string) (SnapshotInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("store: verify snapshot: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return SnapshotInfo{}, fmt.Errorf("%w: not a regular file", ErrNotCoachdDB)
	}
	u, err := fileURI(path, "mode=ro&immutable=1")
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("store: verify snapshot: %w", err)
	}
	sdb, err := sql.Open("sqlite", u)
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("store: verify snapshot: %w", err)
	}
	defer sdb.Close()

	if err := integrityCheck(ctx, sdb); err != nil {
		return SnapshotInfo{}, err
	}
	var v sql.NullString
	if err := sdb.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
		if strings.Contains(err.Error(), "no such table: schema_migrations") {
			return SnapshotInfo{}, ErrNotCoachdDB
		}
		return SnapshotInfo{}, snapshotErr(err)
	}
	if !v.Valid {
		return SnapshotInfo{}, fmt.Errorf("%w: no migrations applied", ErrNotCoachdDB)
	}
	demo, err := readDemo(ctx, sdb)
	if err != nil {
		return SnapshotInfo{}, snapshotErr(err)
	}
	return SnapshotInfo{SchemaVersion: v.String, Demo: demo}, nil
}

// integrityCheck runs PRAGMA integrity_check, which returns the single row
// "ok" for an intact database and one row per problem otherwise.
func integrityCheck(ctx context.Context, sdb *sql.DB) error {
	rows, err := sdb.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return snapshotErr(err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return snapshotErr(err)
		}
		results = append(results, s)
	}
	if err := rows.Err(); err != nil {
		return snapshotErr(err)
	}
	if len(results) != 1 || results[0] != "ok" {
		return fmt.Errorf("%w: %d problems", ErrCorruptSnapshot, len(results))
	}
	return nil
}

// snapshotErr maps "not a database" and "malformed" to ErrCorruptSnapshot.
func snapshotErr(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff { // primary result code
		case sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_CORRUPT:
			return fmt.Errorf("%w: %v", ErrCorruptSnapshot, err)
		}
	}
	return fmt.Errorf("store: verify snapshot: %w", err)
}

// LatestSchemaVersion returns the newest embedded migration, e.g. "0008_deliveries".
func LatestSchemaVersion() string {
	v := migrationVersions()
	return v[len(v)-1]
}

// KnownSchemaVersion reports whether v is one of the embedded migrations.
func KnownSchemaVersion(v string) bool { return slices.Contains(migrationVersions(), v) }
