package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stefiix92/coachd/internal/store"
)

// errLiveDB refuses a restore whose snapshot is the live database itself.
var errLiveDB = errors.New("backup: the snapshot is the live coachd.db; pass a backup file")

// rename moves the database files during a restore; tests replace it.
var rename = os.Rename

// RestoreResult describes a restore.
type RestoreResult struct {
	SchemaVersion   string
	PreRestoreDir   string // "" when there was no previous coachd.db (or -wal or -shm)
	ManifestChecked bool
	FITMissing      int
	FITMismatched   int
	// Carried forward from the previous coachd.db into the restored one.
	RevocationsCarried int
	NoncesCarried      int
	// CarryForwardErr is set when the previous coachd.db could not be read (it
	// may be corrupt): its revocations and used nonces were not carried
	// forward. The restore itself succeeded.
	CarryForwardErr error
}

// Restore replaces <dataDir>/coachd.db with a copy of snapshot. coachd serve
// must be stopped: Restore takes the data lock, then the backup lock. It
// refuses, before coachd.db is touched, a snapshot that is the live database,
// has a non-empty -wal next to it (ErrSnapshotHasWAL), fails
// store.VerifySnapshot, has a schema newer than this binary, or does not
// match the manifest next to it. FIT files that the manifest lists but
// that are missing or changed in dataDir are counted, not refused.
//
// Before the swap, the snapshot's copy gets the previous coachd.db's
// revocations and used approval nonces, loses its web sessions and pending
// enrolments, and gains a backup_restored audit row (store.ApplyCarryForward).
// Reading the previous database is best effort: when it fails (it may be
// corrupt), the restore goes on and CarryForwardErr says so; SQLite may then
// drop an invalid -wal of that database. If the copy cannot be prepared, the
// restore is refused with coachd.db in place (already checkpointed when it was
// readable). The previous coachd.db and its -wal and -shm move to
// <dataDir>/pre-restore-<ts>/; if that or the swap fails, they are moved
// back, best effort.
func Restore(ctx context.Context, dataDir, snapshot string, now time.Time) (RestoreResult, error) {
	live := filepath.Join(dataDir, "coachd.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if sameFile(snapshot, live+suffix) {
			return RestoreResult{}, errLiveDB
		}
	}
	if err := mkdirPrivate(filepath.Join(dataDir, DirName)); err != nil {
		return RestoreResult{}, fmt.Errorf("backup: %w", err)
	}
	releaseData, err := LockData(dataDir)
	if err != nil {
		return RestoreResult{}, err
	}
	defer releaseData()
	releaseBackups, err := lockBackups(filepath.Join(dataDir, DirName))
	if err != nil {
		return RestoreResult{}, err
	}
	defer releaseBackups()

	// VerifySnapshot reads the file alone; commits still in a -wal beside it
	// (a pre-restore-<ts>/ copy, say) would be lost.
	if fi, err := os.Stat(snapshot + "-wal"); err == nil && fi.Size() > 0 {
		return RestoreResult{}, ErrSnapshotHasWAL
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return RestoreResult{}, fmt.Errorf("backup: %w", err)
	}
	info, err := store.VerifySnapshot(ctx, snapshot)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("backup: %w", err)
	}
	if !store.KnownSchemaVersion(info.SchemaVersion) {
		return RestoreResult{}, fmt.Errorf("%w (schema %s, this coachd knows up to %s)", ErrNewerSchema, info.SchemaVersion, store.LatestSchemaVersion())
	}
	res := RestoreResult{SchemaVersion: info.SchemaVersion}
	if err := checkManifest(dataDir, snapshot, &res); err != nil {
		return RestoreResult{}, err
	}
	if err := checkDemo(ctx, live, info.Demo); err != nil {
		return RestoreResult{}, err
	}

	// Checks of the snapshot end here. Reading the previous coachd.db below
	// may checkpoint it; the swap after that replaces it.
	tmp := filepath.Join(dataDir, ".coachd.db.restore.tmp")
	// The copy plus SQLite's journal files: a WAL-mode snapshot (a
	// pre-restore copy) briefly has a -wal while ApplyCarryForward opens it.
	tmpFiles := []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"}
	removeTmp := func() {
		for _, p := range tmpFiles {
			os.Remove(p)
		}
	}
	for _, p := range tmpFiles {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return RestoreResult{}, fmt.Errorf("backup: restore: %w", err)
		}
	}
	in, err := os.Open(snapshot)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("backup: restore: %w", err)
	}
	err = createSynced(tmp, in)
	in.Close()
	if err != nil {
		removeTmp()
		return RestoreResult{}, fmt.Errorf("backup: restore: %w", err)
	}
	var carry store.CarryForward
	if _, err := os.Lstat(live); err == nil {
		carry, res.CarryForwardErr = store.ReadCarryForward(ctx, live)
	} else if !errors.Is(err, fs.ErrNotExist) {
		res.CarryForwardErr = err
	}
	audit := store.AuditEvent{At: now, Lane: "cli", Actor: "cli", Event: "backup_restored", Target: filepath.Base(snapshot), Outcome: "ok"}
	if res.RevocationsCarried, res.NoncesCarried, err = store.ApplyCarryForward(ctx, tmp, carry, audit); err != nil {
		// coachd.db is still in place (checkpointed if it was readable);
		// never install a copy half-prepared.
		removeTmp()
		return RestoreResult{}, fmt.Errorf("backup: restore: %w", err)
	}
	if res.PreRestoreDir, err = moveAside(dataDir, now); err != nil {
		removeTmp()
		if res.PreRestoreDir != "" {
			return rollBack(res, dataDir, err)
		}
		return RestoreResult{}, fmt.Errorf("backup: restore: %w", err)
	}
	if err := rename(tmp, live); err != nil {
		removeTmp()
		return rollBack(res, dataDir, err)
	}
	for _, d := range []string{res.PreRestoreDir, dataDir} {
		if d == "" {
			continue
		}
		if err := syncDir(d); err != nil {
			return res, fmt.Errorf("backup: restore: %w", err)
		}
	}
	return res, nil
}

// checkDemo refuses a snapshot whose demo mark differs from the previous
// coachd.db's. Reading that database may checkpoint it. One that is missing,
// empty or unreadable is not checked: serve checks the mark again at start.
func checkDemo(ctx context.Context, live string, snapshotDemo bool) error {
	if fi, err := os.Stat(live); err != nil || fi.Size() == 0 {
		return nil
	}
	demo, err := store.ReadDemo(ctx, live)
	switch {
	case err != nil || demo == snapshotDemo:
		return nil
	case snapshotDemo:
		return fmt.Errorf("%w: the snapshot holds demo data and coachd.db does not; restore it into a demo data directory", ErrDemoMismatch)
	}
	return fmt.Errorf("%w: coachd.db holds demo data and the snapshot does not; restore it into its own data directory", ErrDemoMismatch)
}

// checkManifest compares the manifest next to snapshot, if there is one, with
// the snapshot (ErrManifestMismatch) and counts its FIT files that are
// missing or different in dataDir.
func checkManifest(dataDir, snapshot string, res *RestoreResult) error {
	body, err := os.ReadFile(strings.TrimSuffix(snapshot, ".db") + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup: manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("backup: manifest: %w", err)
	}
	got, err := hashFile(snapshot)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if got.SHA256 != m.DB.SHA256 {
		return ErrManifestMismatch
	}
	for _, e := range m.FIT {
		rel := filepath.FromSlash(e.Path)
		if !filepath.IsLocal(rel) { // never read outside dataDir
			res.FITMismatched++
			continue
		}
		f, err := hashFile(filepath.Join(dataDir, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.FITMissing++
		case err != nil || f.SHA256 != e.SHA256:
			res.FITMismatched++
		}
	}
	res.ManifestChecked = true
	return nil
}

// moveAside moves whichever of coachd.db, -wal and -shm exist into a new
// <dataDir>/pre-restore-<ts>/ and returns it, or "" when none exist. The
// -wal and -shm go first: an interrupted move must never leave a stale -wal
// beside the restored database, where SQLite would replay it.
func moveAside(dataDir string, now time.Time) (string, error) {
	var present []string
	for _, name := range []string{"coachd.db-wal", "coachd.db-shm", "coachd.db"} {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); err == nil {
			present = append(present, name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	if len(present) == 0 {
		return "", nil
	}
	pre := filepath.Join(dataDir, "pre-restore-"+now.UTC().Format(stampLayout))
	if err := os.Mkdir(pre, 0o700); err != nil {
		return "", err
	}
	for _, name := range present {
		if err := rename(filepath.Join(dataDir, name), filepath.Join(pre, name)); err != nil {
			return pre, err
		}
	}
	return pre, nil
}

// rollBack, after a failed move aside or swap, returns the previous database
// files from res.PreRestoreDir to dataDir, best effort, and returns the
// failure naming that directory. coachd.db goes first: if it cannot go back,
// neither do its -wal and -shm, which SQLite would replay into whatever
// database is created next to them.
func rollBack(res RestoreResult, dataDir string, cause error) (RestoreResult, error) {
	pre := res.PreRestoreDir
	for i, name := range []string{"coachd.db", "coachd.db-wal", "coachd.db-shm"} {
		err := rename(filepath.Join(pre, name), filepath.Join(dataDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue // never moved aside
		}
		if err != nil && i == 0 {
			return res, fmt.Errorf("backup: restore: %w (previous database files are in %s)", cause, pre)
		}
		if err != nil {
			return res, fmt.Errorf("backup: restore: %w (previous coachd.db moved back; move its -wal and -shm back from %s before starting coachd)", cause, pre)
		}
	}
	os.Remove(pre) // empty now
	return RestoreResult{}, fmt.Errorf("backup: restore: %w (previous database files moved back from %s)", cause, pre)
}

// sameFile reports whether a and b name the same file: the same inode when
// both exist (seeing through symlinks and hard links), else the same cleaned
// absolute path.
func sameFile(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	aa, aerr := filepath.Abs(a)
	ba, berr := filepath.Abs(b)
	return aerr == nil && berr == nil && aa == ba
}
