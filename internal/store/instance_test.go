package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDemoMark(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	if demo, err := db.Demo(ctx); err != nil || demo {
		t.Fatalf("new database: demo = %v, %v", demo, err)
	}
	for range 2 { // idempotent
		if err := db.MarkDemo(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if demo, err := db.Demo(ctx); err != nil || !demo {
		t.Fatalf("marked: demo = %v, %v", demo, err)
	}
}

func TestReadDemo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "coachd.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if demo, err := ReadDemo(ctx, path); err != nil || demo {
		t.Fatalf("unmarked: demo = %v, %v", demo, err)
	}
	if err := db.MarkDemo(ctx); err != nil {
		t.Fatal(err)
	}
	// The mark is read while serve's handle is still open (from the -wal).
	if demo, err := ReadDemo(ctx, path); err != nil || !demo {
		t.Fatalf("marked, open: demo = %v, %v", demo, err)
	}
	db.Close()
	if demo, err := ReadDemo(ctx, path); err != nil || !demo {
		t.Fatalf("marked, closed: demo = %v, %v", demo, err)
	}

	// A database from before the instance table is not demo, and reading it
	// does not migrate it.
	old := filepath.Join(dir, "old.db")
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pdb, err := openPlain(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pdb.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	pdb.Close()
	if demo, err := ReadDemo(ctx, old); err != nil || demo {
		t.Fatalf("pre-mark schema: demo = %v, %v", demo, err)
	}
	pdb, _ = openPlain(old)
	defer pdb.Close()
	var n int
	if err := pdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='instance'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ReadDemo migrated the database: %d %v", n, err)
	}

	// Not an SQLite file: an error, never a guess.
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("definitely not a database file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDemo(ctx, junk); err == nil {
		t.Fatal("junk file read as a database")
	}
}

func TestVerifySnapshotReportsDemo(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	plain := filepath.Join(t.TempDir(), "plain.db")
	if err := db.SnapshotTo(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if info, err := VerifySnapshot(ctx, plain); err != nil || info.Demo {
		t.Fatalf("plain snapshot: %+v %v", info, err)
	}
	if err := db.MarkDemo(ctx); err != nil {
		t.Fatal(err)
	}
	demo := filepath.Join(t.TempDir(), "demo.db")
	if err := db.SnapshotTo(ctx, demo); err != nil {
		t.Fatal(err)
	}
	if info, err := VerifySnapshot(ctx, demo); err != nil || !info.Demo {
		t.Fatalf("demo snapshot: %+v %v", info, err)
	}
}
