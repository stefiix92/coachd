package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/store"
)

var restoreAt = t0.Add(48 * time.Hour)

func addToken(t *testing.T, db *store.DB, id string) {
	t.Helper()
	if err := db.CreateToken(context.Background(), store.TokenRecord{ID: id, Name: id, Hash: []byte(id), CreatedAt: t0, ExpiresAt: t0}); err != nil {
		t.Fatal(err)
	}
}

func tokenIDs(t *testing.T, db *store.DB) []string {
	t.Helper()
	list, err := db.ListTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tok := range list {
		ids = append(ids, tok.ID)
	}
	return ids
}

// backedUp returns a data directory holding one backup of a database with
// token "t1", the backup's Result and the closed live database's bytes.
func backedUp(t *testing.T) (string, Result, []byte) {
	t.Helper()
	dataDir, db := setup(t)
	addToken(t, db, "t1")
	res, err := Run(context.Background(), db, dataDir, "v-test", t0)
	if err != nil {
		t.Fatal(err)
	}
	addToken(t, db, "t2")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(filepath.Join(dataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	return dataDir, res, live
}

// assertUntouched fails unless coachd.db still holds live and no
// pre-restore directory was created.
func assertUntouched(t *testing.T, dataDir string, live []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dataDir, "coachd.db"))
	if err != nil || !bytes.Equal(got, live) {
		t.Fatalf("coachd.db changed (err=%v)", err)
	}
	if m, _ := filepath.Glob(filepath.Join(dataDir, "pre-restore-*")); len(m) != 0 {
		t.Fatalf("pre-restore dirs %v", m)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ".coachd.db.restore.tmp")); !os.IsNotExist(err) {
		t.Fatalf("restore temp file exists (err=%v)", err)
	}
}

func TestRestoreReplacesDatabase(t *testing.T) {
	dataDir, res, live := backedUp(t)
	ctx := context.Background()
	rr, err := Restore(ctx, dataDir, res.Snapshot, restoreAt)
	if err != nil {
		t.Fatal(err)
	}
	if rr.SchemaVersion != store.LatestSchemaVersion() || !rr.ManifestChecked || rr.FITMissing != 0 || rr.FITMismatched != 0 {
		t.Fatalf("result %+v", rr)
	}
	if rr.PreRestoreDir != filepath.Join(dataDir, "pre-restore-20261026T031500Z") {
		t.Fatalf("PreRestoreDir %q", rr.PreRestoreDir)
	}
	if fi, err := os.Stat(rr.PreRestoreDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("pre-restore dir mode %v err=%v", fi.Mode(), err)
	}
	old, err := os.ReadFile(filepath.Join(rr.PreRestoreDir, "coachd.db"))
	if err != nil || !bytes.Equal(old, live) {
		t.Fatalf("pre-restore coachd.db differs from the old database (err=%v)", err)
	}
	if fi, err := os.Stat(filepath.Join(dataDir, "coachd.db")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("coachd.db mode %v err=%v", fi.Mode(), err)
	}
	db, err := store.Open(ctx, filepath.Join(dataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ids := tokenIDs(t, db); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("tokens after restore %v, want [t1]", ids)
	}
}

func TestRestoreIntoEmptyDataDir(t *testing.T) {
	_, res, _ := backedUp(t)
	dataDir := filepath.Join(t.TempDir(), "fresh")
	rr, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
	if err != nil {
		t.Fatal(err)
	}
	if rr.PreRestoreDir != "" || !rr.ManifestChecked || rr.FITMissing != len(fitFiles) {
		t.Fatalf("result %+v", rr)
	}
	if _, err := store.VerifySnapshot(context.Background(), filepath.Join(dataDir, "coachd.db")); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreMovesWALAndSHM: SQLite cannot open the corrupt previous
// database, so its -wal and -shm are still there to move aside. (A readable
// one is checkpointed first; see TestRestoreCarriesSecurityStateForward.)
func TestRestoreMovesWALAndSHM(t *testing.T) {
	dataDir, res, _ := backedUp(t)
	writeFile(t, filepath.Join(dataDir, "coachd.db"), strings.Repeat("not a database ", 1000))
	for _, suffix := range []string{"-wal", "-shm"} {
		writeFile(t, filepath.Join(dataDir, "coachd.db"+suffix), "stale "+suffix)
	}
	rr, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"coachd.db", "coachd.db-wal", "coachd.db-shm"} {
		if _, err := os.Stat(filepath.Join(rr.PreRestoreDir, name)); err != nil {
			t.Fatalf("%s not moved: %v", name, err)
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dataDir, "coachd.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("coachd.db%s still in data dir (err=%v)", suffix, err)
		}
	}
}

func TestRestoreRefusesWhileLocked(t *testing.T) {
	dataDir, res, live := backedUp(t)
	release, err := LockData(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := LockData(dataDir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second LockData: %v, want ErrLocked", err)
	}
	if _, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt); !errors.Is(err, ErrLocked) {
		t.Fatalf("err %v, want ErrLocked", err)
	}
	assertUntouched(t, dataDir, live)
}

func TestRestoreRefusesWhileBackupRuns(t *testing.T) {
	dataDir, res, live := backedUp(t)
	release, err := lockBackups(filepath.Join(dataDir, DirName))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt); !errors.Is(err, ErrBusy) {
		t.Fatalf("err %v, want ErrBusy", err)
	}
	assertUntouched(t, dataDir, live)
}

func TestRestoreRefusesCorrupt(t *testing.T) {
	dataDir, _, live := backedUp(t)
	bad := filepath.Join(t.TempDir(), "coachd-20261024T031500Z.db")
	writeFile(t, bad, strings.Repeat("not a database ", 1000))
	if _, err := Restore(context.Background(), dataDir, bad, restoreAt); !errors.Is(err, store.ErrCorruptSnapshot) {
		t.Fatalf("err %v, want ErrCorruptSnapshot", err)
	}
	assertUntouched(t, dataDir, live)
}

func TestRestoreRefusesNewerSchema(t *testing.T) {
	dataDir, res, live := backedUp(t)
	future := filepath.Join(t.TempDir(), "future.db")
	b, err := os.ReadFile(res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(future, b, 0o600); err != nil {
		t.Fatal(err)
	}
	fdb, err := sql.Open("sqlite", future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fdb.Exec(`INSERT INTO schema_migrations(version) VALUES ('9999_future')`); err != nil {
		t.Fatal(err)
	}
	if err := fdb.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), dataDir, future, restoreAt); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("err %v, want ErrNewerSchema", err)
	}
	assertUntouched(t, dataDir, live)
}

func TestRestoreRefusesManifestMismatch(t *testing.T) {
	dataDir, res, live := backedUp(t)
	m := readManifest(t, res.Manifest)
	m.DB.SHA256 = strings.Repeat("0", 64)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(res.Manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt); !errors.Is(err, ErrManifestMismatch) {
		t.Fatalf("err %v, want ErrManifestMismatch", err)
	}
	assertUntouched(t, dataDir, live)
}

func TestRestoreRefusesLiveDB(t *testing.T) {
	dataDir, _, live := backedUp(t)
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(filepath.Join(dataDir, "coachd.db"), link); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []string{
		filepath.Join(dataDir, "coachd.db"),
		dataDir + "/./fit/../coachd.db",
		link,
		filepath.Join(dataDir, "coachd.db-wal"), // does not exist; compared by path
	} {
		if _, err := Restore(context.Background(), dataDir, snapshot, restoreAt); !errors.Is(err, errLiveDB) {
			t.Fatalf("%s: err %v, want errLiveDB", snapshot, err)
		}
		assertUntouched(t, dataDir, live)
	}
}

func TestRestoreReportsFITDrift(t *testing.T) {
	dataDir, res, _ := backedUp(t)
	if err := os.Remove(filepath.Join(dataDir, "fit", "2026", "101.fit")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dataDir, "fit", "2026", "102.fit"), "changed")
	rr, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.ManifestChecked || rr.FITMissing != 1 || rr.FITMismatched != 1 {
		t.Fatalf("result %+v", rr)
	}
}

func TestRestoreWithoutManifest(t *testing.T) {
	dataDir, res, _ := backedUp(t)
	if err := os.Remove(res.Manifest); err != nil {
		t.Fatal(err)
	}
	rr, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
	if err != nil {
		t.Fatal(err)
	}
	if rr.ManifestChecked || rr.FITMissing != 0 || rr.FITMismatched != 0 || rr.PreRestoreDir == "" {
		t.Fatalf("result %+v", rr)
	}
}

// security holds one of each credential the restore carries forward.
type security struct {
	token    string
	passkey  int64
	device   int64
	sessHash []byte
}

// addSecurity creates a token, a passkey with a web session, a device and a
// pending enrolment in db.
func addSecurity(t *testing.T, db *store.DB) security {
	t.Helper()
	ctx := context.Background()
	addToken(t, db, "sec-token")
	enr, err := db.CreateEnrollment(ctx, []byte("enrol-used"), t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pk, err := db.EnrollPasskey(ctx, store.EnrollParams{EnrollmentID: enr, Nonce: "enrol-nonce", NonceExpires: t0.Add(time.Hour),
		Passkey: store.Passkey{CredentialID: []byte("cred-1"), CredentialJSON: []byte(`{}`), Label: "phone"}, At: t0, Lane: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateEnrollment(ctx, []byte("enrol-pending"), t0, t0.Add(1000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, []byte("session-1"), pk, t0, t0.Add(1000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePairing(ctx, store.Pairing{ID: "pairing-1", Code: "123456", CreatedAt: t0, ExpiresAt: t0.Add(10 * time.Minute)}, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConfirmPairing(ctx, "123456", "watch", t0, "cli", "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	_, dev, err := db.ClaimPairing(ctx, "pairing-1", []byte("device-token-1"), t0)
	if err != nil {
		t.Fatal(err)
	}
	return security{token: "sec-token", passkey: pk, device: dev, sessHash: []byte("session-1")}
}

// revokeAll revokes every credential in s and uses an approval nonce.
func revokeAll(t *testing.T, db *store.DB, s security) {
	t.Helper()
	ctx, at := context.Background(), t0.Add(time.Hour)
	if err := db.RevokeToken(ctx, s.token, at); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokePasskey(ctx, s.passkey, at, "public", "passkey:1"); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeDevice(ctx, s.device, at, "cli", "cli", "cli"); err != nil {
		t.Fatal(err)
	}
	if err := db.ConsumeNonce(ctx, "approval-after-snapshot", at, at.Add(1000*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, path, query string) int {
	t.Helper()
	sdb, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	var n int
	if err := sdb.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestRestoreCarriesSecurityStateForward(t *testing.T) {
	dataDir, db := setup(t)
	ctx := context.Background()
	s := addSecurity(t, db)
	res, err := Run(ctx, db, dataDir, "v-test", t0)
	if err != nil {
		t.Fatal(err)
	}
	revokeAll(t, db, s)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rr, err := Restore(ctx, dataDir, res.Snapshot, restoreAt)
	if err != nil {
		t.Fatal(err)
	}
	if rr.CarryForwardErr != nil || rr.RevocationsCarried != 3 || rr.NoncesCarried != 1 {
		t.Fatalf("result %+v", rr)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(rr.PreRestoreDir, "coachd.db"+suffix)); !os.IsNotExist(err) {
			t.Errorf("pre-restore coachd.db%s exists (err=%v): the previous database is not self-contained", suffix, err)
		}
	}
	live := filepath.Join(dataDir, "coachd.db")
	if n := count(t, live, `SELECT count(*) FROM web_sessions`); n != 0 {
		t.Errorf("%d web sessions after restore", n)
	}
	if n := count(t, live, `SELECT count(*) FROM enrollments WHERE used_at IS NULL`); n != 0 {
		t.Errorf("%d pending enrolments after restore", n)
	}
	rdb, err := store.Open(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	toks, err := rdb.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range toks {
		if tok.ID == s.token && tok.RevokedAt == nil {
			t.Error("token revoked after the snapshot is active again")
		}
	}
	pks, err := rdb.ListPasskeys(ctx)
	if err != nil || len(pks) != 1 || pks[0].RevokedAt == nil {
		t.Errorf("passkeys %+v err=%v, want the one passkey revoked", pks, err)
	}
	devs, err := rdb.ListDevices(ctx)
	if err != nil || len(devs) != 1 || devs[0].RevokedAt == nil {
		t.Errorf("devices %+v err=%v, want the one device revoked", devs, err)
	}
	if used, err := rdb.NonceUsed(ctx, "approval-after-snapshot"); err != nil || !used {
		t.Errorf("nonce used after the snapshot: used=%v err=%v", used, err)
	}
	events, err := rdb.ListAudit(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := store.AuditEvent{At: restoreAt, Lane: "cli", Actor: "cli", Event: "backup_restored", Target: filepath.Base(res.Snapshot), Outcome: "ok"}
	if len(events) != 1 || events[0] != want {
		t.Errorf("newest audit row %+v, want %+v", events, want)
	}
}

func TestRestoreWithCorruptLiveDB(t *testing.T) {
	for name, garbage := range map[string]string{
		"no SQLite header":   strings.Repeat("not a database ", 1000),
		"SQLite header only": "SQLite format 3\x00" + strings.Repeat("not a database ", 1000),
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, res, _ := backedUp(t)
			writeFile(t, filepath.Join(dataDir, "coachd.db"), garbage)
			rr, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
			if err != nil {
				t.Fatal(err)
			}
			if rr.CarryForwardErr == nil || rr.RevocationsCarried != 0 || rr.NoncesCarried != 0 {
				t.Fatalf("result %+v, want a CarryForwardErr", rr)
			}
			if old, err := os.ReadFile(filepath.Join(rr.PreRestoreDir, "coachd.db")); err != nil || string(old) != garbage {
				t.Fatalf("pre-restore coachd.db is not the corrupt database (err=%v)", err)
			}
			live := filepath.Join(dataDir, "coachd.db")
			if n := count(t, live, `SELECT count(*) FROM audit_log WHERE event='backup_restored'`); n != 1 {
				t.Errorf("%d backup_restored audit rows, want 1", n)
			}
			db, err := store.Open(context.Background(), live)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if ids := tokenIDs(t, db); len(ids) != 1 || ids[0] != "t1" {
				t.Fatalf("tokens after restore %v, want [t1]", ids)
			}
		})
	}
}

// TestRestoreOlderSchema restores a snapshot from before the devices
// migration: the device revocation is skipped, the token revocation is not.
func TestRestoreOlderSchema(t *testing.T) {
	dataDir, db := setup(t)
	ctx := context.Background()
	s := addSecurity(t, db)
	res, err := Run(ctx, db, dataDir, "v-test", t0)
	if err != nil {
		t.Fatal(err)
	}
	revokeAll(t, db, s)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(t.TempDir(), "coachd-20250101T000000Z.db")
	b, err := os.ReadFile(res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, b, 0o600); err != nil {
		t.Fatal(err)
	}
	odb, err := sql.Open("sqlite", old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := odb.Exec(`ALTER TABLE activities DROP COLUMN perceived_rpe; ALTER TABLE activities DROP COLUMN perceived_feel;
		ALTER TABLE notes DROP COLUMN valid_until; ALTER TABLE activity_detail DROP COLUMN workout_fp;
		ALTER TABLE activity_detail DROP COLUMN workout_json; DROP TABLE messages; DROP TABLE instance;
		DROP TABLE oauth_tokens; DROP TABLE oauth_codes; DROP TABLE oauth_grants; DROP TABLE oauth_clients;
		DROP TABLE deliveries; DROP TABLE device_pairings; DROP TABLE devices;
		DELETE FROM schema_migrations WHERE version >= '0007'`); err != nil {
		t.Fatal(err)
	}
	if err := odb.Close(); err != nil {
		t.Fatal(err)
	}
	rr, err := Restore(ctx, dataDir, old, restoreAt)
	if err != nil {
		t.Fatal(err)
	}
	if rr.SchemaVersion != "0006_proposal_trace" || rr.CarryForwardErr != nil || rr.RevocationsCarried != 2 || rr.NoncesCarried != 1 {
		t.Fatalf("result %+v", rr)
	}
	rdb, err := store.Open(ctx, filepath.Join(dataDir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	if devs, err := rdb.ListDevices(ctx); err != nil || len(devs) != 0 {
		t.Fatalf("devices %+v err=%v", devs, err)
	}
}

// TestRestoreRefusesUnpreparableCopy: when the snapshot's copy cannot take
// the carry-forward (here a trigger aborts the audit insert), the restore
// fails before coachd.db is touched rather than install it half-prepared.
func TestRestoreRefusesUnpreparableCopy(t *testing.T) {
	dataDir, res, live := backedUp(t)
	bad := filepath.Join(t.TempDir(), "coachd-20250101T000000Z.db")
	b, err := os.ReadFile(res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, b, 0o600); err != nil {
		t.Fatal(err)
	}
	bdb, err := sql.Open("sqlite", bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bdb.Exec(`CREATE TRIGGER no_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'no audit'); END`); err != nil {
		t.Fatal(err)
	}
	if err := bdb.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), dataDir, bad, restoreAt); err == nil || !strings.Contains(err.Error(), "no audit") {
		t.Fatalf("err %v, want the trigger's abort", err)
	}
	assertUntouched(t, dataDir, live)
}

func TestRestoreRefusesSnapshotWithWAL(t *testing.T) {
	dataDir, res, live := backedUp(t)
	writeFile(t, res.Snapshot+"-wal", "uncommitted frames")
	if _, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt); !errors.Is(err, ErrSnapshotHasWAL) {
		t.Fatalf("err %v, want ErrSnapshotHasWAL", err)
	}
	assertUntouched(t, dataDir, live)
	// An empty -wal holds nothing: the restore goes ahead.
	writeFile(t, res.Snapshot+"-wal", "")
	if _, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt); err != nil {
		t.Fatalf("with an empty -wal: %v", err)
	}
}

// corruptLive replaces the live database and its -wal and -shm with
// unreadable files (left alone by the carry-forward) and returns their
// contents by name.
func corruptLive(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	files := map[string]string{
		"coachd.db":     strings.Repeat("not a database ", 1000),
		"coachd.db-wal": "stale -wal",
		"coachd.db-shm": "stale -shm",
	}
	for name, body := range files {
		writeFile(t, filepath.Join(dataDir, name), body)
	}
	return files
}

// failRename makes rename fail for the moves fail selects and records every
// call as "src -> dst" with paths relative to dataDir.
func failRename(t *testing.T, dataDir string, fail func(src, dst string) bool) *[]string {
	t.Helper()
	var calls []string
	rename = func(src, dst string) error {
		rs, _ := filepath.Rel(dataDir, src)
		rd, _ := filepath.Rel(dataDir, dst)
		calls = append(calls, rs+" -> "+rd)
		if fail(src, dst) {
			return errors.New("injected rename failure")
		}
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { rename = os.Rename })
	return &calls
}

func assertFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	for name, body := range want {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != body {
			t.Errorf("%s: %q err=%v, want the original", name, got, err)
		}
	}
}

func TestRestoreRollsBackWhenFinalRenameFails(t *testing.T) {
	dataDir, res, _ := backedUp(t)
	files := corruptLive(t, dataDir)
	tmp := filepath.Join(dataDir, ".coachd.db.restore.tmp")
	calls := failRename(t, dataDir, func(src, _ string) bool { return src == tmp })
	pre := filepath.Join(dataDir, "pre-restore-20261026T031500Z")
	_, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
	if err == nil || !strings.Contains(err.Error(), "injected rename failure") || !strings.Contains(err.Error(), "moved back from "+pre) {
		t.Fatalf("err %v", err)
	}
	assertFiles(t, dataDir, files)
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("restore temp file exists (err=%v)", err)
	}
	p := "pre-restore-20261026T031500Z/"
	want := []string{
		"coachd.db-wal -> " + p + "coachd.db-wal", "coachd.db-shm -> " + p + "coachd.db-shm", "coachd.db -> " + p + "coachd.db",
		".coachd.db.restore.tmp -> coachd.db",
		p + "coachd.db -> coachd.db", p + "coachd.db-wal -> coachd.db-wal", p + "coachd.db-shm -> coachd.db-shm",
	}
	if !slices.Equal(*calls, want) {
		t.Errorf("renames\n got %q\nwant %q", *calls, want)
	}
}

func TestRestoreRollsBackWhenMoveAsideFails(t *testing.T) {
	dataDir, res, _ := backedUp(t)
	files := corruptLive(t, dataDir)
	live := filepath.Join(dataDir, "coachd.db")
	failRename(t, dataDir, func(src, _ string) bool { return src == live })
	_, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
	if err == nil || !strings.Contains(err.Error(), "injected rename failure") || !strings.Contains(err.Error(), "moved back from ") {
		t.Fatalf("err %v", err)
	}
	assertFiles(t, dataDir, files)
}

// TestRestoreRollbackKeepsWALWithItsDatabase: when coachd.db cannot move
// back, its -wal and -shm stay with it in pre-restore-<ts>/.
func TestRestoreRollbackKeepsWALWithItsDatabase(t *testing.T) {
	dataDir, res, _ := backedUp(t)
	files := corruptLive(t, dataDir)
	tmp, live := filepath.Join(dataDir, ".coachd.db.restore.tmp"), filepath.Join(dataDir, "coachd.db")
	failRename(t, dataDir, func(src, dst string) bool { return src == tmp || dst == live })
	pre := filepath.Join(dataDir, "pre-restore-20261026T031500Z")
	_, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
	if err == nil || !strings.Contains(err.Error(), "previous database files are in "+pre) {
		t.Fatalf("err %v", err)
	}
	assertFiles(t, pre, files)
	for _, name := range []string{"coachd.db", "coachd.db-wal", "coachd.db-shm"} {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s in the data dir (err=%v)", name, err)
		}
	}
}

// TestRestoreRollbackReportsStrandedWAL: when coachd.db moves back but its
// -wal cannot, the error says so instead of claiming every file is in
// pre-restore-<ts>/.
func TestRestoreRollbackReportsStrandedWAL(t *testing.T) {
	dataDir, res, _ := backedUp(t)
	corruptLive(t, dataDir)
	tmp := filepath.Join(dataDir, ".coachd.db.restore.tmp")
	pre := filepath.Join(dataDir, "pre-restore-20261026T031500Z")
	failRename(t, dataDir, func(src, _ string) bool { return src == tmp || src == filepath.Join(pre, "coachd.db-wal") })
	_, err := Restore(context.Background(), dataDir, res.Snapshot, restoreAt)
	if err == nil || !strings.Contains(err.Error(), "previous coachd.db moved back; move its -wal and -shm back from "+pre) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "coachd.db")); err != nil {
		t.Errorf("coachd.db not moved back: %v", err)
	}
}

// Demo data and the runner's data never replace each other; a demo backup
// restores into a demo or an empty data directory.
func TestRestoreKeepsDemoAndRealApart(t *testing.T) {
	ctx := context.Background()
	demoBackup := func(t *testing.T) string {
		t.Helper()
		dir, db := setup(t)
		if err := db.MarkDemo(ctx); err != nil {
			t.Fatal(err)
		}
		res, err := Run(ctx, db, dir, "v-test", t0)
		if err != nil {
			t.Fatal(err)
		}
		return res.Snapshot
	}

	// A demo snapshot into a real data directory.
	realDir, _, live := backedUp(t)
	if _, err := Restore(ctx, realDir, demoBackup(t), restoreAt); !errors.Is(err, ErrDemoMismatch) {
		t.Fatalf("demo into real: err = %v", err)
	}
	assertUntouched(t, realDir, live)

	// A real snapshot into a demo data directory.
	dir, res, _ := backedUp(t)
	db, err := store.Open(ctx, filepath.Join(dir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDemo(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	live, err = os.ReadFile(filepath.Join(dir, "coachd.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dir, res.Snapshot, restoreAt); !errors.Is(err, ErrDemoMismatch) {
		t.Fatalf("real into demo: err = %v", err)
	}
	assertUntouched(t, dir, live)

	// A demo snapshot into an empty directory, and into a demo one.
	empty := t.TempDir()
	snap := demoBackup(t)
	if _, err := Restore(ctx, empty, snap, restoreAt); err != nil {
		t.Fatalf("demo into empty: %v", err)
	}
	if demo, err := store.ReadDemo(ctx, filepath.Join(empty, "coachd.db")); err != nil || !demo {
		t.Fatalf("restored database not demo: %v %v", demo, err)
	}
	if _, err := Restore(ctx, empty, snap, restoreAt.Add(time.Hour)); err != nil {
		t.Fatalf("demo into demo: %v", err)
	}
}
