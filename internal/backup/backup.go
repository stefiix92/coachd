// Package backup writes and restores coachd backups in <DataDir>/backups: a
// consistent SQLite snapshot plus a JSON manifest that lists it and every FIT
// file with its SHA-256. A backup is complete only when both
// coachd-<ts>.db and coachd-<ts>.json exist; the manifest is written last.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stefiix92/coachd/internal/store"
)

const (
	DirName  = "backups"      // under DataDir
	Keep     = 7              // complete backups kept by retention
	Interval = 24 * time.Hour // a backup is due when the newest is this old
)

var (
	ErrBusy             = errors.New("backup: another backup or restore is running")
	ErrLocked           = errors.New("backup: data directory is in use by another coachd process")
	ErrNewerSchema      = errors.New("backup: snapshot is from a newer coachd")
	ErrManifestMismatch = errors.New("backup: manifest does not match the snapshot")
	ErrExists           = errors.New("backup: a backup with this timestamp already exists")
	ErrSnapshotHasWAL   = errors.New("backup: the snapshot has a non-empty -wal file next to it")
	// ErrDemoMismatch refuses a restore that would put demo data where the
	// runner's data is, or the reverse (docs/design/demo-mode.md).
	ErrDemoMismatch = errors.New("backup: demo data and real data never replace each other")
)

// FileEntry is one file in a manifest.
type FileEntry struct {
	Path   string `json:"path"` // relative to DataDir, forward slashes, e.g. "fit/2026/123.fit"
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"` // lowercase hex
}

// Manifest is the coachd-<ts>.json written next to each snapshot.
type Manifest struct {
	CreatedAt     time.Time   `json:"created_at"` // UTC
	Version       string      `json:"coachd_version"`
	SchemaVersion string      `json:"schema_version"`
	DB            FileEntry   `json:"db"`  // Path is the snapshot's file name, e.g. "coachd-20261024T031500Z.db"
	FIT           []FileEntry `json:"fit"` // sorted by Path
}

// Result describes a backup written by Run.
type Result struct {
	Snapshot, Manifest string // absolute paths
	DBBytes            int64
	FITFiles           int
	FITBytes           int64
	Pruned             int // complete backups removed by retention
}

// stampLayout is the UTC time in backup and pre-restore names.
const stampLayout = "20060102T150405Z"

var namePattern = regexp.MustCompile(`^coachd-\d{8}T\d{6}Z$`)

// Run writes a backup named after now: a snapshot of db and a manifest of
// the FIT files under dataDir. It then removes complete backups beyond the
// newest Keep, never the one it just wrote, and snapshots left without a
// manifest. A failed run leaves no new backup and prunes nothing. If only
// pruning fails, Run returns the Result of the new, complete backup with the
// error.
func Run(ctx context.Context, db *store.DB, dataDir, version string, now time.Time) (Result, error) {
	return run(ctx, func() (*store.DB, error) { return db, nil }, false, dataDir, version, now)
}

// RunWith is Run for a caller without an open database (coachd backup): it
// takes the backup lock first, then calls open, and closes the database
// before releasing the lock. While another backup or restore holds the lock
// it returns ErrBusy without calling open.
func RunWith(ctx context.Context, open func() (*store.DB, error), dataDir, version string, now time.Time) (Result, error) {
	return run(ctx, open, true, dataDir, version, now)
}

func run(ctx context.Context, open func() (*store.DB, error), closeDB bool, dataDir, version string, now time.Time) (Result, error) {
	dir, err := filepath.Abs(filepath.Join(dataDir, DirName))
	if err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	if err := mkdirPrivate(dir); err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	release, err := lockBackups(dir)
	if err != nil {
		return Result{}, err
	}
	defer release()
	db, err := open()
	if err != nil {
		return Result{}, fmt.Errorf("backup: open store: %w", err)
	}
	if closeDB {
		defer db.Close() // runs before release
	}
	if err := removeTemps(dir); err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	name := "coachd-" + now.UTC().Format(stampLayout)
	for _, ext := range []string{".db", ".json"} {
		if _, err := os.Lstat(filepath.Join(dir, name+ext)); err == nil {
			return Result{}, ErrExists
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("backup: %w", err)
		}
	}
	res, err := write(ctx, db, dataDir, dir, name, version, now)
	if err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	if res.Pruned, err = prune(dir, name); err != nil {
		return res, fmt.Errorf("backup: prune: %w", err)
	}
	return res, nil
}

// write snapshots db, hashes it and the FIT files, and publishes the backup
// by renaming the snapshot and then the manifest into place. On error it
// removes every file of this backup, including any it already renamed.
func write(ctx context.Context, db *store.DB, dataDir, dir, name, version string, now time.Time) (res Result, err error) {
	snap, man := filepath.Join(dir, name+".db"), filepath.Join(dir, name+".json")
	tmpSnap, tmpMan := filepath.Join(dir, "."+name+".db.tmp"), filepath.Join(dir, "."+name+".json.tmp")
	defer func() {
		if err != nil {
			// Manifest first: an interrupted cleanup leaves at most a snapshot
			// without a manifest, which the next prune removes.
			for _, p := range []string{man, tmpMan, snap, tmpSnap} {
				os.Remove(p)
			}
		}
	}()
	if err := db.SnapshotTo(ctx, tmpSnap); err != nil {
		return Result{}, err
	}
	if err := chmodSync(tmpSnap); err != nil {
		return Result{}, err
	}
	info, err := store.VerifySnapshot(ctx, tmpSnap)
	if err != nil {
		return Result{}, err
	}
	dbEntry, err := hashFile(tmpSnap)
	if err != nil {
		return Result{}, err
	}
	dbEntry.Path = name + ".db"
	fit, err := fitEntries(ctx, dataDir)
	if err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmpSnap, snap); err != nil {
		return Result{}, err
	}
	body, err := json.MarshalIndent(Manifest{
		CreatedAt: now.UTC(), Version: version, SchemaVersion: info.SchemaVersion, DB: dbEntry, FIT: fit,
	}, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := createSynced(tmpMan, bytes.NewReader(append(body, '\n'))); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmpMan, man); err != nil {
		return Result{}, err
	}
	if err := syncDir(dir); err != nil {
		return Result{}, err
	}
	res = Result{Snapshot: snap, Manifest: man, DBBytes: dbEntry.Size, FITFiles: len(fit)}
	for _, e := range fit {
		res.FITBytes += e.Size
	}
	return res, nil
}

// hashFIT hashes one FIT file; tests replace it.
var hashFIT = hashFile

// fitEntries hashes the regular files under <dataDir>/fit, sorted by path.
// Symlinks are skipped, not followed; a missing fit directory has no entries.
// Entries named .* or *.tmp (ingest's in-progress downloads, hidden
// directories) are skipped, and so is a file that vanishes before its hash.
func fitEntries(ctx context.Context, dataDir string) ([]FileEntry, error) {
	root := filepath.Join(dataDir, "fit")
	entries := []FileEntry{} // "fit": [] rather than null
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp")) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		e, err := hashFIT(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return err
		}
		e.Path = filepath.ToSlash(rel)
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fit: %w", err)
	}
	slices.SortFunc(entries, func(a, b FileEntry) int { return strings.Compare(a.Path, b.Path) })
	return entries, nil
}

// Newest returns the time in the name of the newest complete backup; ok is
// false when there is none (or no backups directory yet).
func Newest(dataDir string) (t time.Time, ok bool, err error) {
	names, err := complete(filepath.Join(dataDir, DirName))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("backup: %w", err)
	}
	if len(names) == 0 {
		return time.Time{}, false, nil
	}
	t, _ = stamp(names[len(names)-1])
	return t, true, nil
}

// complete returns the base names of the complete backups in dir, oldest
// first: a coachd-<ts>.db and a coachd-<ts>.json, both regular files.
func complete(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir) // sorted by name, which for backups is by time
	if err != nil {
		return nil, err
	}
	regular := map[string]bool{}
	for _, e := range entries {
		regular[e.Name()] = e.Type().IsRegular()
	}
	var names []string
	for _, e := range entries {
		base, isDB := strings.CutSuffix(e.Name(), ".db")
		if _, ok := stamp(base); ok && isDB && regular[e.Name()] && regular[base+".json"] {
			names = append(names, base)
		}
	}
	return names, nil
}

// stamp parses the time in a backup base name, coachd-20060102T150405Z.
func stamp(base string) (time.Time, bool) {
	if !namePattern.MatchString(base) {
		return time.Time{}, false
	}
	t, err := time.Parse(stampLayout, strings.TrimPrefix(base, "coachd-"))
	return t, err == nil
}

// prune removes complete backups beyond the newest Keep, manifest first, then
// snapshots without a manifest: crash leftovers, since the backup lock rules
// out a run in progress. The backup named keep, just written, always counts
// among the Keep, even when a clock that went back names it before the
// others. It returns how many complete backups it removed.
func prune(dir, keep string) (int, error) {
	names, err := complete(dir)
	if err != nil {
		return 0, err
	}
	names = slices.DeleteFunc(names, func(n string) bool { return n == keep })
	pruned := 0
	for _, base := range names[:max(len(names)-(Keep-1), 0)] {
		for _, ext := range []string{".json", ".db"} {
			if err := os.Remove(filepath.Join(dir, base+ext)); err != nil {
				return pruned, err
			}
		}
		pruned++
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return pruned, err
	}
	for _, e := range entries {
		base, isDB := strings.CutSuffix(e.Name(), ".db")
		if _, ok := stamp(base); !ok || !isDB || !e.Type().IsRegular() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, base+".json")); errors.Is(err, fs.ErrNotExist) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return pruned, err
			}
		}
	}
	return pruned, nil
}

// removeTemps deletes the .*.tmp files an interrupted run left in dir.
func removeTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".tmp") && e.Type().IsRegular() {
			if err := os.Remove(filepath.Join(dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// mkdirPrivate creates dir and its parents and sets dir to mode 0700, also
// when it already existed with a wider mode.
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// hashFile returns the size and SHA-256 of the file at path (Path unset).
func hashFile(path string) (FileEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileEntry{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileEntry{}, err
	}
	return FileEntry{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// createSynced writes r to a new file at path (mode 0600) and flushes it to disk.
func createSynced(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// chmodSync sets the file at path to mode 0600 and flushes it to disk.
func chmodSync(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Chmod(0o600)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// syncDir flushes dir's entries, making renames into or out of it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
