package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/muktihari/fit/profile/typedef"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/fitdata/fittest"
	"github.com/stefiix92/coachd/internal/garmin"
	"github.com/stefiix92/coachd/internal/intervals"
	"github.com/stefiix92/coachd/internal/store"
)

func TestDrainFITWritesFileAtomically(t *testing.T) {
	f := seededFake(t)
	body, err := fittest.Build(fittest.Options{Start: time.Date(2026, 10, 22, 5, 0, 0, 0, time.UTC), Laps: 2, Records: 60, HR: 150, SpeedMS: 3})
	if err != nil {
		t.Fatal(err)
	}
	f.fit[idRun] = body
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)

	a, err := e.db.GetActivity(ctx, idRun)
	if err != nil {
		t.Fatal(err)
	}
	if a.FITStatus != store.FITStored || a.FITPath != "fit/2026/1001.fit" {
		t.Fatalf("fit status/path = %q %q, want stored fit/2026/1001.fit (relative to DataDir)", a.FITStatus, a.FITPath)
	}
	abs := filepath.Join(e.dir, filepath.FromSlash(a.FITPath))
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("stored FIT bytes differ from download")
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %04o, want 0600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(abs))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %04o, want 0700", perm)
	}
	entries, err := os.ReadDir(filepath.Dir(abs))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "1001.fit" {
		t.Errorf("dir entries = %v, want only 1001.fit (no temp files left)", entries)
	}
}

func TestDrainFITFailuresAreBounded(t *testing.T) {
	f := seededFake(t)
	f.fitErr[idRun] = &garmin.StatusError{Endpoint: "activity_file", Status: 500}
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	for i := 0; i < 5; i++ {
		e.svc.drainFIT(ctx)
	}
	if n := f.fitCount(idRun); n != 3 {
		t.Fatalf("failing FIT requested %d times over 5 drains, want 3 (one per drain until 3 attempts)", n)
	}
	a, err := e.db.GetActivity(ctx, idRun)
	if err != nil {
		t.Fatal(err)
	}
	if a.FITStatus != store.FITFailed || a.FITAttempts != 3 || a.FITPath != "" {
		t.Fatalf("activity = status %q attempts %d path %q", a.FITStatus, a.FITAttempts, a.FITPath)
	}
}

func TestDrainFITEmptyBodyFails(t *testing.T) {
	f := seededFake(t)
	f.fit[idRun] = []byte{}
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	a, err := e.db.GetActivity(ctx, idRun)
	if err != nil {
		t.Fatal(err)
	}
	if a.FITStatus != store.FITFailed || a.FITPath != "" {
		t.Fatalf("empty body: status %q path %q, want failed", a.FITStatus, a.FITPath)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "fit", "2026", "1001.fit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty body must not write a file: %v", err)
	}
}

func TestDrainFITStopsOnRateLimit(t *testing.T) {
	f := seededFake(t)
	f.fitErr[idRun] = &garmin.RateLimitedError{RetryAt: testNow.Add(time.Hour)}
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 1 {
		t.Fatalf("FIT calls = %d, want 1 (drain stops at the 429)", n)
	}
	a, err := e.db.GetActivity(ctx, idRun)
	if err != nil {
		t.Fatal(err)
	}
	if a.FITStatus != store.FITPending || a.FITAttempts != 0 {
		t.Fatalf("rate-limited FIT = %q attempts %d, want pending 0", a.FITStatus, a.FITAttempts)
	}
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 1 {
		t.Fatalf("drain while paused made %d more calls", n-1)
	}
	if r, err := e.svc.Sync(ctx); err != nil || r.Outcomes["activities"] != OutcomeRateLimited {
		t.Fatalf("sync after FIT 429 = %v %v, want paused", r.Outcomes, err)
	}
}

func TestDrainFITStopsOnAuthFailure(t *testing.T) {
	f := seededFake(t)
	f.fitErr[idRun] = garmin.ErrUnauthorized
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 1 {
		t.Fatalf("FIT calls = %d, want 1 (no retry storm on auth failure)", n)
	}
	a, err := e.db.GetActivity(ctx, idRun)
	if err != nil {
		t.Fatal(err)
	}
	if a.FITStatus != store.FITPending || a.FITAttempts != 0 {
		t.Fatalf("auth-failed FIT = %q attempts %d, want pending 0 (activity not at fault)", a.FITStatus, a.FITAttempts)
	}
}

func TestDrainFITReachesActivitiesBeyondFirstBatch(t *testing.T) {
	f := newFake()
	// 24 activities on one day, listed by id desc: odd ids fail, even ids succeed,
	// so failures never run consecutively and failed rows stay in the query result.
	for i := 0; i < 24; i++ {
		id := int64(2000 + i)
		f.activities = append(f.activities, item(t, activityDTO(id, "running", "2026-10-22 07:00:00", "2026-10-22 05:00:00", 1000, false)))
		if id%2 == 1 {
			f.fitErr[id] = &garmin.StatusError{Endpoint: "activity_file", Status: 500}
		} else {
			f.fit[id] = []byte("FIT")
		}
	}
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 24 {
		t.Fatalf("FIT calls = %d, want 24 (each activity once per drain)", n)
	}
	for i := 0; i < 24; i++ {
		id := int64(2000 + i)
		a, err := e.db.GetActivity(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := store.FITStored
		if id%2 == 1 {
			want = store.FITFailed
		}
		if a.FITStatus != want || f.fitCount(id) != 1 {
			t.Errorf("activity %d: fit %q calls %d, want %q 1", id, a.FITStatus, f.fitCount(id), want)
		}
	}
}

// pendingFake returns n activities on distinct days (newest first by id order),
// all with FIT error err.
func pendingFake(t *testing.T, n int, err error) (*fakeGarmin, []int64) {
	t.Helper()
	f := newFake()
	var ids []int64
	for i := 0; i < n; i++ {
		id := int64(4000 + i)
		day := fmt.Sprintf("2026-10-%02d", 22-i)
		f.activities = append(f.activities, item(t, activityDTO(id, "running", day+" 07:00:00", day+" 05:00:00", 1000, false)))
		f.fitErr[id] = err
		ids = append(ids, id)
	}
	return f, ids
}

func TestDrainFITStopsOnTransportError(t *testing.T) {
	f, ids := pendingFake(t, 5, &garmin.StatusError{Endpoint: "activity_file", Status: 0})
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 1 {
		t.Fatalf("FIT calls = %d, want 1 (transport failure stops the drain)", n)
	}
	for _, id := range ids {
		a, err := e.db.GetActivity(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.FITStatus != store.FITPending || a.FITAttempts != 0 {
			t.Errorf("activity %d: fit %q attempts %d, want pending 0", id, a.FITStatus, a.FITAttempts)
		}
	}
}

func TestDrainFITStopsAfterTwoConsecutiveFailures(t *testing.T) {
	f, ids := pendingFake(t, 5, &garmin.StatusError{Endpoint: "activity_file", Status: 503})
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 2 {
		t.Fatalf("FIT calls = %d, want 2", n)
	}
	for i, id := range ids {
		a, err := e.db.GetActivity(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want, attempts := store.FITPending, 0
		if i < 2 {
			want, attempts = store.FITFailed, 1
		}
		if a.FITStatus != want || a.FITAttempts != attempts {
			t.Errorf("activity %d: fit %q attempts %d, want %q %d", id, a.FITStatus, a.FITAttempts, want, attempts)
		}
	}
}

func TestDrainFITSuccessResetsFailureStreak(t *testing.T) {
	boom := &garmin.StatusError{Endpoint: "activity_file", Status: 500}
	f, ids := pendingFake(t, 6, boom)
	// Newest first: fail, ok, 404, fail, fail, (not reached).
	delete(f.fitErr, ids[1])
	f.fit[ids[1]] = []byte("FIT")
	f.fitErr[ids[2]] = garmin.ErrNotFound
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 5 {
		t.Fatalf("FIT calls = %d, want 5 (success resets the streak; 404 does not count)", n)
	}
	if f.fitCount(ids[5]) != 0 {
		t.Fatal("drain should stop before the sixth activity")
	}
}

func buildFIT(t *testing.T, records int) []byte {
	t.Helper()
	b, err := fittest.Build(fittest.Options{Start: time.Date(2026, 10, 22, 5, 0, 0, 0, time.UTC), Laps: 3, Records: records, HR: 150, SpeedMS: 3})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDrainStoresDetail(t *testing.T) {
	f := seededFake(t)
	f.fit[idRun] = buildFIT(t, 5000)
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)

	d, err := e.db.GetActivityDetail(ctx, idRun)
	if err != nil {
		t.Fatalf("detail row missing after FIT download: %v", err)
	}
	if len(d.Series) == 0 || len(d.Series) > 600 || len(d.Laps) != 3 {
		t.Fatalf("series=%d laps=%d, want 1..600 and 3", len(d.Series), len(d.Laps))
	}
}

func TestDrainStoresWorkout(t *testing.T) {
	wf, err := fittest.WorkoutFile("Easy", []fittest.Step{{Intensity: typedef.IntensityActive,
		Duration: typedef.WktStepDurationTime, DurationValue: 600000, Target: typedef.WktStepTargetHeartRate, Low: 230, High: 260}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := fittest.Build(fittest.Options{Start: time.Date(2026, 10, 22, 5, 0, 0, 0, time.UTC), Records: 600, HR: 150,
		SpeedMS: 3, LapRecords: []int{600}, LapSteps: []uint16{0}, Workout: wf})
	if err != nil {
		t.Fatal(err)
	}
	f := seededFake(t)
	f.fit[idRun] = body
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)

	d, err := e.db.GetActivityDetail(ctx, idRun)
	if err != nil {
		t.Fatal(err)
	}
	if d.Workout == nil || len(d.Workout.Executed) != 1 || d.Workout.Executed[0].TargetTime == nil ||
		d.Workout.Executed[0].TargetTime.InS != 600 {
		t.Fatalf("workout = %+v", d.Workout)
	}
	a, err := e.db.GetActivity(ctx, idRun)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := e.db.ActivityWorkouts(ctx, a.Date, a.Date)
	if err != nil || len(ws) != 1 || ws[0].ID != idRun || ws[0].Fingerprint != d.Workout.Fingerprint {
		t.Fatalf("activity workouts = %+v, %v", ws, err)
	}
}

func TestDrainFillsMissingDetail(t *testing.T) {
	f := seededFake(t)
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	// A stored FIT from before detail existed: file on disk, no detail row.
	rel := "fit/2026/1001.fit"
	abs := filepath.Join(e.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, buildFIT(t, 120), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetFIT(ctx, idRun, rel, store.FITStored); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.GetActivityDetail(ctx, idRun); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("precondition: detail err = %v", err)
	}
	e.svc.drainFIT(ctx)
	d, err := e.db.GetActivityDetail(ctx, idRun)
	if err != nil || len(d.Series) != 120 || len(d.Laps) != 3 {
		t.Fatalf("detail not filled: err=%v series=%d laps=%d", err, len(d.Series), len(d.Laps))
	}
}

func TestDrainFillMissingDetailStopsOnCancel(t *testing.T) {
	f := seededFake(t)
	e := newEnv(t, f, nil)
	ctx := context.Background()
	mustSync(t, e)
	rel := "fit/2026/1001.fit"
	abs := filepath.Join(e.dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(abs), 0o700)
	os.WriteFile(abs, buildFIT(t, 50), 0o600)
	e.db.SetFIT(ctx, idRun, rel, store.FITStored)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	e.svc.fillMissingDetail(cctx)
	if _, err := e.db.GetActivityDetail(ctx, idRun); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fill ran with cancelled ctx: %v", err)
	}
}

// storedWithoutDetail stores activity id dated date with a stored FIT file
// holding body (written under DataDir) and no detail row, as from before
// detail existed. A nil body leaves the file missing.
func storedWithoutDetail(t *testing.T, e *env, id int64, date string, body []byte) {
	t.Helper()
	ctx := context.Background()
	it := item(t, activityDTO(id, "running", date+" 07:00:00", date+" 05:00:00", 5000, false))
	a, err := MapActivity(it.V, e.svc.opts.TZ)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.UpsertActivity(ctx, a, it.Raw); err != nil {
		t.Fatal(err)
	}
	rel := path.Join("fit", date[:4], strconv.FormatInt(id, 10)+".fit")
	if body != nil {
		abs := filepath.Join(e.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.db.SetFIT(ctx, id, rel, store.FITStored); err != nil {
		t.Fatal(err)
	}
}

func hasDetail(t *testing.T, e *env, id int64) bool {
	t.Helper()
	_, err := e.db.GetActivityDetail(context.Background(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// Local detail fill runs after the downloads, so it never delays them.
func TestDrainFillsDetailAfterDownloads(t *testing.T) {
	f := newFake()
	e := newEnv(t, f, nil)
	storedWithoutDetail(t, e, 10, "2026-10-20", buildFIT(t, 60))
	it := item(t, activityDTO(11, "running", "2026-10-21 07:00:00", "2026-10-21 05:00:00", 5000, false))
	a, err := MapActivity(it.V, e.svc.opts.TZ)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.UpsertActivity(context.Background(), a, it.Raw); err != nil {
		t.Fatal(err)
	}
	f.fit[11] = buildFIT(t, 60)
	filledFirst := false
	f.setHook(func(m string) {
		if m == mFIT {
			filledFirst = hasDetail(t, e, 10)
		}
	})
	e.svc.drainFIT(context.Background())
	if n := f.count(mFIT); n != 1 {
		t.Fatalf("FIT downloads = %d, want 1", n)
	}
	if filledFirst {
		t.Fatal("detail fill ran before the FIT download")
	}
	if !hasDetail(t, e, 10) || !hasDetail(t, e, 11) {
		t.Fatalf("detail: filled %v, downloaded %v; want both", hasDetail(t, e, 10), hasDetail(t, e, 11))
	}
}

// Local decodes are not paced by BackfillDelay.
func TestDrainFillDoesNotSleepBetweenDecodes(t *testing.T) {
	e := newEnv(t, newFake(), nil)
	e.svc.opts.BackfillDelay = time.Hour
	for i := range 3 {
		storedWithoutDetail(t, e, int64(20+i), fmt.Sprintf("2026-10-%02d", 18+i), buildFIT(t, 60))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e.svc.drainFIT(ctx)
	for i := range 3 {
		if !hasDetail(t, e, int64(20+i)) {
			t.Errorf("activity %d: no detail, want every local decode done without pacing", 20+i)
		}
	}
}

// The fill makes no Garmin call, so a Garmin pause does not stop it.
func TestDrainFillIgnoresGarminPause(t *testing.T) {
	f := newFake()
	e := newEnv(t, f, nil)
	storedWithoutDetail(t, e, 30, "2026-10-20", buildFIT(t, 60))
	e.svc.notePause(context.Background(), &garmin.RateLimitedError{RetryAt: testNow.Add(time.Hour)})
	if _, paused := e.svc.pausedUntil(); !paused {
		t.Fatal("precondition: not paused")
	}
	e.svc.drainFIT(context.Background())
	if !hasDetail(t, e, 30) {
		t.Fatal("no detail: the fill stopped on a Garmin pause")
	}
	if n := f.totalCalls(); n != 0 {
		t.Fatalf("Garmin calls = %d, want 0 while paused", n)
	}
}

// A FIT whose fill failed (missing or undecodable) is not retried by later
// drains of the same process.
func TestDrainFillSkipsFailedFITs(t *testing.T) {
	logs := newLogRecorder()
	e := newEnv(t, newFake(), logs)
	storedWithoutDetail(t, e, 40, "2026-10-20", nil)                 // file missing
	storedWithoutDetail(t, e, 41, "2026-10-21", []byte("not a FIT")) // undecodable
	for range 3 {
		e.svc.drainFIT(context.Background())
	}
	fails := logs.lines(t, "fit detail failed")
	if len(fails) != 2 {
		t.Fatalf("%d 'fit detail failed' lines over 3 drains, want 2 (one per bad FIT)", len(fails))
	}
	got := map[string]bool{}
	for _, l := range fails {
		got[l.attrs["activity_id"]] = true
	}
	if !got["40"] || !got["41"] {
		t.Fatalf("failed ids %v, want 40 and 41", got)
	}
}

// Failed FITs do not use up the per-drain cap of later drains, and one drain
// decodes at most detailFillBatch.
func TestDrainFillFailedDoNotBlockOthers(t *testing.T) {
	logs := newLogRecorder()
	e := newEnv(t, newFake(), logs)
	storedWithoutDetail(t, e, 50, "2026-09-01", buildFIT(t, 60)) // oldest: listed last
	for i := range detailFillBatch {
		storedWithoutDetail(t, e, int64(100+i), "2026-10-20", []byte("bad"))
	}
	e.svc.drainFIT(context.Background())
	if n := len(logs.lines(t, "fit detail failed")); n != detailFillBatch || detailFillBatch != 20 {
		t.Fatalf("first drain: %d failures (cap %d), want 20", n, detailFillBatch)
	}
	if hasDetail(t, e, 50) {
		t.Fatal("first drain decoded more than the cap")
	}
	e.svc.drainFIT(context.Background())
	if !hasDetail(t, e, 50) {
		t.Fatal("second drain: the good FIT is still blocked by failed ones")
	}
	if n := len(logs.lines(t, "fit detail failed")); n != detailFillBatch {
		t.Fatalf("second drain retried failed FITs: %d failure lines, want %d", n, detailFillBatch)
	}
}

// An activity row that vanishes during its download leaves no orphan file and
// does not count toward the failure streak.
func TestDrainFITVanishedRowRemovesFile(t *testing.T) {
	f, ids := pendingFake(t, 3, nil) // newest first: ids[0], ids[1], ids[2]
	for _, id := range ids {
		delete(f.fitErr, id)
		f.fit[id] = []byte("FIT")
	}
	logs := newLogRecorder()
	e := newEnv(t, f, logs)
	ctx := context.Background()
	mustSync(t, e)
	sdb := rawDB(t, e)
	calls := 0
	f.setHook(func(m string) {
		if m != mFIT {
			return
		}
		if calls++; calls <= 2 { // the rows of the first two downloads vanish mid-download
			if _, err := sdb.Exec(`DELETE FROM activities WHERE id=?`, ids[calls-1]); err != nil {
				t.Error(err)
			}
		}
	})
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 3 {
		t.Fatalf("FIT calls = %d, want 3 (vanished rows are not failures)", n)
	}
	for _, id := range ids[:2] {
		p := filepath.Join(e.dir, "fit", "2026", strconv.FormatInt(id, 10)+".fit")
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("orphan FIT %s: stat err = %v, want removed", p, err)
		}
	}
	a, err := e.db.GetActivity(ctx, ids[2])
	if err != nil || a.FITStatus != store.FITStored {
		t.Fatalf("third activity = %+v %v, want stored", a, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(a.FITPath))); err != nil {
		t.Fatalf("third FIT file: %v", err)
	}
	var skipped int
	for _, l := range logs.lines(t, "fit download finished") {
		if l.attrs["outcome"] == "skipped" {
			wantAttrs(t, l, map[string]string{"data_type": "fit", "reason": "activity_missing"})
			skipped++
		}
	}
	if skipped != 2 {
		t.Fatalf("%d skipped download lines, want 2", skipped)
	}
}

// A row soft-deleted (reconcile) while its FIT download is in flight keeps
// the FIT: soft-deleted activities retain their raw JSON and FIT file, so
// SetFIT records it and nothing is treated as an orphan.
func TestDrainFITSoftDeletedMidDownloadKeepsFile(t *testing.T) {
	f, ids := pendingFake(t, 1, nil)
	id := ids[0]
	delete(f.fitErr, id)
	f.fit[id] = []byte("FIT")
	logs := newLogRecorder()
	e := newEnv(t, f, logs)
	ctx := context.Background()
	mustSync(t, e)
	f.setHook(func(m string) {
		if m == mFIT {
			if err := e.db.MarkActivityDeleted(ctx, id, time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC)); err != nil {
				t.Error(err)
			}
		}
	})
	e.svc.drainFIT(ctx)
	if n := f.count(mFIT); n != 1 {
		t.Fatalf("FIT calls = %d, want 1", n)
	}
	p := filepath.Join(e.dir, "fit", "2026", strconv.FormatInt(id, 10)+".fit")
	if b, err := os.ReadFile(p); err != nil || string(b) != "FIT" {
		t.Fatalf("FIT file %s = %q, %v; want kept", p, b, err)
	}
	var status, rel string
	var deleted bool
	if err := rawDB(t, e).QueryRow(`SELECT fit_status, fit_path, deleted_at IS NOT NULL FROM activities WHERE id=?`, id).
		Scan(&status, &rel, &deleted); err != nil {
		t.Fatal(err)
	}
	if status != string(store.FITStored) || rel != "fit/2026/"+strconv.FormatInt(id, 10)+".fit" || !deleted {
		t.Fatalf("row fit_status %q fit_path %q deleted %v, want stored, the file, still deleted", status, rel, deleted)
	}
	for _, l := range logs.lines(t, "fit download finished") {
		if l.attrs["outcome"] != string(store.FITStored) {
			t.Fatalf("download logged %v, want stored (no orphan removal)", l.attrs)
		}
	}
	if n := len(logs.lines(t, "fit download finished")); n != 1 {
		t.Fatalf("%d download lines, want 1", n)
	}
	if l := logs.lines(t, "fit file not removed"); len(l) != 0 {
		t.Fatalf("orphan removal attempted: %v", l)
	}
}

// recordEvents records ActivityStored calls.
type recordEvents struct {
	mu  sync.Mutex
	ids []int64
	// detail is whether the activity's detail was stored when it was told.
	detail []bool
	db     *store.DB
}

func (r *recordEvents) ActivityStored(ctx context.Context, a store.Activity) {
	_, err := r.db.GetActivityDetail(ctx, a.ID)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, a.ID)
	r.detail = append(r.detail, err == nil)
}

func TestDrainTellsEventsOfRecentActivities(t *testing.T) {
	f := seededFake(t) // idRun started 27 h before testNow; idGone 65 h before, and its FIT is gone (404)
	f.fit[idRun] = buildFIT(t, 600)
	e := newEnv(t, f, nil)
	ev := &recordEvents{db: e.db}
	e.svc.opts.Events = ev
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)
	if len(ev.ids) != 1 || ev.ids[0] != idRun || !ev.detail[0] {
		t.Fatalf("events %v (detail stored: %v), want only the recent run, after its detail", ev.ids, ev.detail)
	}
	// A later drain does not tell it again.
	e.svc.drainFIT(ctx)
	if len(ev.ids) != 1 {
		t.Fatalf("events after a second drain: %v", ev.ids)
	}
}

func TestDrainTellsEventsOfUnavailableFIT(t *testing.T) {
	f := seededFake(t)
	f.fit[idRun] = buildFIT(t, 600)
	e := newEnv(t, f, nil)
	e.clock.Set(time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)) // idGone (404) is 3 h old
	ev := &recordEvents{db: e.db}
	e.svc.opts.Events = ev
	mustSync(t, e)
	e.svc.drainFIT(context.Background())
	if !slices.Contains(ev.ids, idGone) {
		t.Fatalf("events %v, want the activity whose FIT is unavailable", ev.ids)
	}
}

func TestDrainFailedFITTellsNothing(t *testing.T) {
	f := seededFake(t)
	f.fitErr[idRun] = &garmin.StatusError{Endpoint: "activity_file", Status: 500}
	e := newEnv(t, f, nil)
	ev := &recordEvents{db: e.db}
	e.svc.opts.Events = ev
	mustSync(t, e)
	e.svc.drainFIT(context.Background())
	if slices.Contains(ev.ids, idRun) {
		t.Fatalf("events %v: a failed download must not be told", ev.ids)
	}
}

// Fixture intervals.icu ride, 16 h before testNow (recent enough for Events).
const idRide = "i55751783"

var rideDate = civil.Date{Year: 2026, Month: 10, Day: 22}

// fitDay is the i-th day back from 2026-10-22, for one activity per day.
func fitDay(i int) civil.Date { return rideDate.AddDays(-i) }

// pendingGarmin stores Garmin run id on date with its FIT pending, as a sync would.
func pendingGarmin(t *testing.T, e *env, id int64, date civil.Date) {
	t.Helper()
	it := item(t, activityDTO(id, "running", date.String()+" 07:00:00", date.String()+" 05:00:00", 5000, false))
	a, err := MapActivity(it.V, e.svc.opts.TZ)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.UpsertActivity(context.Background(), a, it.Raw); err != nil {
		t.Fatal(err)
	}
}

// pendingIntervals stores intervals.icu ride sourceID on date with its FIT
// pending, as a sync would, and returns its coachd ID.
func pendingIntervals(t *testing.T, e *env, sourceID string, date civil.Date) int64 {
	t.Helper()
	it := intervalsItem(t, rideOn(sourceID, date))
	a, err := MapIntervalsActivity(it.V, e.svc.opts.TZ)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.UpsertActivity(context.Background(), a, it.Raw); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// wantFIT fails unless activity id has FIT status, path and attempts as given.
func wantFIT(t *testing.T, e *env, id int64, status store.FITStatus, rel string, attempts int) {
	t.Helper()
	a, err := e.db.GetActivity(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.FITStatus != status || a.FITPath != rel || a.FITAttempts != attempts {
		t.Errorf("activity %d: fit %q path %q attempts %d, want %q %q %d", id, a.FITStatus, a.FITPath, a.FITAttempts, status, rel, attempts)
	}
}

// An intervals.icu file is named by its source ID, which must be the
// i<digits> form of the activity's ID before it becomes part of a path.
func TestFITFileName(t *testing.T) {
	for _, c := range []struct {
		a    store.Activity
		want string
	}{
		{store.Activity{ID: 1001, Source: store.SourceGarmin}, "1001.fit"},
		{store.Activity{ID: -55751783, Source: store.SourceIntervals, SourceID: "i55751783"}, "i55751783.fit"},
	} {
		if got, err := fitFileName(c.a); err != nil || got != c.want {
			t.Errorf("fitFileName(%d, %q) = %q, %v; want %q", c.a.ID, c.a.SourceID, got, err, c.want)
		}
	}
	for _, sid := range []string{"", "../i55751783", "i55751783/..", "i5575178", "55751783", "i055751783"} {
		a := store.Activity{ID: -55751783, Source: store.SourceIntervals, SourceID: sid}
		if got, err := fitFileName(a); err == nil {
			t.Errorf("fitFileName(source id %q) = %q, want an error", sid, got)
		}
	}
}

func TestFetchFITIntervalsStored(t *testing.T) {
	body := buildFIT(t, 600)
	fi := newFakeIntervals()
	fi.setItems(intervalsItem(t, rideOn(idRide, rideDate)))
	fi.files[idRide] = body
	g := newFake()
	e := newIntervalsEnv(t, g, fi, nil)
	ev := &recordEvents{db: e.db}
	e.svc.opts.Events = ev
	ctx := context.Background()
	mustSync(t, e)
	e.svc.drainFIT(ctx)

	const id int64 = -55751783
	wantFIT(t, e, id, store.FITStored, "fit/2026/i55751783.fit", 0)
	abs := filepath.Join(e.dir, "fit", "2026", "i55751783.fit")
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("stored FIT bytes differ from the intervals.icu download")
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %04o, want 0600", perm)
	}
	if n := fi.fileCount(idRide); n != 1 || g.count(mFIT) != 0 {
		t.Errorf("downloads: intervals.icu %d, Garmin %d; want 1 and 0", n, g.count(mFIT))
	}
	if d, err := e.db.GetActivityDetail(ctx, id); err != nil || len(d.Laps) != 3 || len(d.Series) == 0 {
		t.Fatalf("detail = %d laps, %d points, %v; want 3 laps and a series", len(d.Laps), len(d.Series), err)
	}
	if len(ev.ids) != 1 || ev.ids[0] != id || !ev.detail[0] {
		t.Fatalf("events %v (detail stored: %v), want the ride, after its detail", ev.ids, ev.detail)
	}
}

// A body that is not a whole FIT file is unavailable, not failed: the
// original never will be one. Nothing is written and it is not asked again.
func TestFetchFITIntervalsNotFIT(t *testing.T) {
	whole := buildFIT(t, 60)
	for name, body := range map[string][]byte{
		"html":      []byte("<html>"),
		"gpx":       []byte(`<?xml version="1.0"?><gpx version="1.1"><trk></trk></gpx>`),
		"empty":     {},
		"truncated": whole[:len(whole)-1],
	} {
		t.Run(name, func(t *testing.T) {
			fi := newFakeIntervals()
			fi.files[idRide] = body
			e := newIntervalsEnv(t, newFake(), fi, nil)
			id := pendingIntervals(t, e, idRide, rideDate)
			ctx := context.Background()
			e.svc.drainFIT(ctx)
			e.svc.drainFIT(ctx)
			wantFIT(t, e, id, store.FITUnavailable, "", 0)
			if n := fi.fileCount(idRide); n != 1 {
				t.Errorf("file requested %d times over two drains, want 1", n)
			}
			if _, err := os.Stat(filepath.Join(e.dir, "fit")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("fit/ exists (%v), want nothing written", err)
			}
		})
	}
}

func TestFetchFITIntervalsNotFound(t *testing.T) {
	fi := newFakeIntervals() // no file: 404
	e := newIntervalsEnv(t, newFake(), fi, nil)
	ev := &recordEvents{db: e.db}
	e.svc.opts.Events = ev
	id := pendingIntervals(t, e, idRide, rideDate)
	ctx := context.Background()
	e.svc.drainFIT(ctx)
	e.svc.drainFIT(ctx)
	wantFIT(t, e, id, store.FITUnavailable, "", 0)
	if n := fi.fileCount(idRide); n != 1 {
		t.Errorf("file requested %d times over two drains, want 1", n)
	}
	if !slices.Equal(ev.ids, []int64{id}) {
		t.Errorf("events %v, want the ride once (unavailable, as for Garmin)", ev.ids)
	}
}

// When one source stops (429, rejected key, unreachable, or failing
// maxFITFailureStreak times in a row), the drain leaves its activities
// untouched and keeps downloading the other source's, in both directions.
func TestDrainContinuesWhenOneSourcePaused(t *testing.T) {
	body := buildFIT(t, 60)
	retry := testNow.Add(time.Hour)
	cases := []struct {
		name                    string
		garminErr, intervalsErr error
		garminOut, intervalsOut string // drain span outcome
		calls                   int    // downloads from the stopping source
		failed                  bool   // its downloads count as failed attempts
	}{
		{"rate limited", &garmin.RateLimitedError{RetryAt: retry}, &intervals.RateLimitedError{RetryAt: retry},
			OutcomeRateLimited, OutcomeIntervalsRateLimited, 1, false},
		{"unauthorized", garmin.ErrUnauthorized, intervals.ErrUnauthorized, OutcomeAuthFailed, OutcomeAuthFailed, 1, false},
		{"unreachable", &garmin.StatusError{Endpoint: "activity_file"}, &intervals.StatusError{Endpoint: "file"},
			OutcomeError, OutcomeError, 1, false},
		{"failing", &garmin.StatusError{Endpoint: "activity_file", Status: 503}, &intervals.StatusError{Endpoint: "file", Status: 503},
			"stopped", "stopped", maxFITFailureStreak, true},
	}
	// Newest first: one activity of the source that keeps going, 2*fitBatch of
	// the source that stops (its skipped rows outnumber a batch plus those
	// tried), then two more of the one that keeps going.
	stopsAt := func(i int) bool { return i >= 1 && i <= 2*fitBatch }
	const rows = 2*fitBatch + 3
	for _, stopping := range []store.Source{store.SourceGarmin, store.SourceIntervals} {
		for _, c := range cases {
			t.Run(string(stopping)+"/"+c.name, func(t *testing.T) {
				g, fi := newFake(), newFakeIntervals()
				logs := newLogRecorder()
				e := newIntervalsEnv(t, g, fi, logs)
				sr := traced(t, e)
				var stopped, going []int64
				for i := range rows {
					stops := stopsAt(i)
					var id int64
					if (stops && stopping == store.SourceGarmin) || (!stops && stopping == store.SourceIntervals) {
						id = int64(5000 + i)
						pendingGarmin(t, e, id, fitDay(i))
						if stops {
							g.fitErr[id] = c.garminErr
						} else {
							g.fit[id] = body
						}
					} else {
						sid := "i" + strconv.Itoa(6000+i)
						id = pendingIntervals(t, e, sid, fitDay(i))
						if stops {
							fi.fileErr[sid] = c.intervalsErr
						} else {
							fi.files[sid] = body
						}
					}
					if stops {
						stopped = append(stopped, id)
					} else {
						going = append(going, id)
					}
				}
				e.svc.drainFIT(context.Background())

				for _, id := range going {
					a, err := e.db.GetActivity(context.Background(), id)
					if err != nil || a.FITStatus != store.FITStored {
						t.Errorf("activity %d of the source that keeps going: %q %v, want stored", id, a.FITStatus, err)
					}
				}
				for i, id := range stopped {
					if c.failed && i < c.calls {
						wantFIT(t, e, id, store.FITFailed, "", 1)
					} else {
						wantFIT(t, e, id, store.FITPending, "", 0)
					}
				}
				gCalls, iCalls := g.count(mFIT), fi.count(miFile)
				wantG, wantI, wantOut := len(going), c.calls, c.intervalsOut
				if stopping == store.SourceGarmin {
					wantG, wantI, wantOut = c.calls, len(going), c.garminOut
				}
				if gCalls != wantG || iCalls != wantI {
					t.Errorf("downloads: Garmin %d, intervals.icu %d; want %d and %d", gCalls, iCalls, wantG, wantI)
				}
				_, gPaused := e.svc.pausedUntil()
				_, iPaused := e.svc.intervalsPausedUntil()
				rateLimited := c.name == "rate limited"
				if gPaused != (rateLimited && stopping == store.SourceGarmin) || iPaused != (rateLimited && stopping == store.SourceIntervals) {
					t.Errorf("paused: Garmin %v, intervals.icu %v; want only the stopping source's, on a 429", gPaused, iPaused)
				}
				drains := spansNamed(sr, "ingest.fit.drain")
				if len(drains) != 1 {
					t.Fatalf("%d drain spans, want 1", len(drains))
				}
				if out, _ := attr(drains[0], "outcome"); out != wantOut {
					t.Errorf("drain outcome = %q, want %q", out, wantOut)
				}
				if n := len(logs.lines(t, "fit download finished")); n != gCalls+iCalls {
					t.Errorf("%d download lines, want %d", n, gCalls+iCalls)
				}
			})
		}
	}
}

// Once every source is stopped the drain ends, and paused sources are not
// called again by a later drain.
func TestDrainStopsWhenEverySourceStopped(t *testing.T) {
	g, fi := newFake(), newFakeIntervals()
	e := newIntervalsEnv(t, g, fi, nil)
	for i := range 6 {
		if i%2 == 0 {
			id := int64(5000 + i)
			pendingGarmin(t, e, id, fitDay(i))
			g.fitErr[id] = &garmin.RateLimitedError{RetryAt: testNow.Add(time.Hour)}
		} else {
			sid := "i" + strconv.Itoa(6000+i)
			pendingIntervals(t, e, sid, fitDay(i))
			fi.fileErr[sid] = &intervals.RateLimitedError{RetryAt: testNow.Add(time.Hour)}
		}
	}
	e.svc.drainFIT(context.Background())
	if gc, ic := g.count(mFIT), fi.count(miFile); gc != 1 || ic != 1 {
		t.Fatalf("downloads: Garmin %d, intervals.icu %d; want 1 each", gc, ic)
	}
	e.svc.drainFIT(context.Background())
	if gc, ic := g.count(mFIT), fi.count(miFile); gc != 1 || ic != 1 {
		t.Fatalf("downloads while both paused: Garmin %d, intervals.icu %d; want no more", gc-1, ic-1)
	}
}

// Each source has its own failure streak: the other source's successes in
// between do not reset it.
func TestDrainFailureStreakIsPerSource(t *testing.T) {
	body := buildFIT(t, 60)
	g, fi := newFake(), newFakeIntervals()
	e := newIntervalsEnv(t, g, fi, nil)
	for i := range 6 { // newest first: Garmin 503, intervals.icu ok, alternating
		if i%2 == 0 {
			id := int64(5000 + i)
			pendingGarmin(t, e, id, fitDay(i))
			g.fitErr[id] = &garmin.StatusError{Endpoint: "activity_file", Status: 503}
		} else {
			sid := "i" + strconv.Itoa(6000+i)
			pendingIntervals(t, e, sid, fitDay(i))
			fi.files[sid] = body
		}
	}
	e.svc.drainFIT(context.Background())
	if gc, ic := g.count(mFIT), fi.count(miFile); gc != maxFITFailureStreak || ic != 3 {
		t.Fatalf("downloads: Garmin %d, intervals.icu %d; want %d and 3", gc, ic, maxFITFailureStreak)
	}
}

// Pending intervals.icu activities left from an earlier configuration are
// skipped, not failed, and do not keep the drain from Garmin's.
func TestDrainSkipsIntervalsWhenOff(t *testing.T) {
	body := buildFIT(t, 60)
	g, fi := newFake(), newFakeIntervals()
	e := newIntervalsEnv(t, g, fi, nil)
	sr := traced(t, e)
	var rides []int64
	for i := range fitBatch + 2 { // newer than the Garmin run, past a whole batch
		sid := "i" + strconv.Itoa(7000+i)
		fi.files[sid] = body
		rides = append(rides, pendingIntervals(t, e, sid, fitDay(i)))
	}
	pendingGarmin(t, e, idRun, fitDay(fitBatch+2))
	g.fit[idRun] = body
	e.svc.opts.Intervals = nil
	e.svc.drainFIT(context.Background())

	wantFIT(t, e, idRun, store.FITStored, "fit/2026/1001.fit", 0)
	for _, id := range rides {
		wantFIT(t, e, id, store.FITPending, "", 0)
	}
	if n := fi.count(miFile); n != 0 {
		t.Errorf("intervals.icu downloads = %d while the source is off, want 0", n)
	}
	drains := spansNamed(sr, "ingest.fit.drain")
	if len(drains) != 1 {
		t.Fatalf("%d drain spans, want 1", len(drains))
	}
	out, _ := attr(drains[0], "outcome")
	count, _ := attr(drains[0], "count")
	if out != OutcomeOK || count != "1" {
		t.Errorf("drain outcome %q count %q, want ok 1", out, count)
	}
}

// A hidden copy's FIT is still downloaded with its detail, but Events hears
// only of the shown copy of a session, as a hidden copy is skipped like a
// soft-deleted one.
func TestDrainTellsEventsOfShownCopyOnly(t *testing.T) {
	e, fi := dupSetup(t, fp(200), nil) // tie on power: the Garmin copy 5001 wins, i777 is hidden
	e.fake.fit[5001] = buildFIT(t, 600)
	fi.files["i777"] = buildFIT(t, 600)
	ev := &recordEvents{db: e.db}
	e.svc.opts.Events = ev
	mustSync(t, e)
	var by sql.NullInt64
	if err := rawDB(t, e).QueryRow(`SELECT hidden_by FROM activities WHERE id=-777`).Scan(&by); err != nil || !by.Valid || by.Int64 != 5001 {
		t.Fatalf("precondition: intervals copy hidden_by = %v, %v, want 5001", by, err)
	}
	e.svc.drainFIT(context.Background())

	wantFIT(t, e, 5001, store.FITStored, "fit/2026/5001.fit", 0)
	var status, rel string
	if err := rawDB(t, e).QueryRow(`SELECT fit_status, fit_path FROM activities WHERE id=-777`).Scan(&status, &rel); err != nil ||
		status != string(store.FITStored) || rel != "fit/2026/i777.fit" {
		t.Fatalf("hidden copy fit %q path %q, %v; want stored fit/2026/i777.fit", status, rel, err)
	}
	var hiddenDetail int // reads skip a hidden copy, so count its row directly
	if err := rawDB(t, e).QueryRow(`SELECT COUNT(*) FROM activity_detail WHERE activity_id=-777`).Scan(&hiddenDetail); err != nil {
		t.Fatal(err)
	}
	if !hasDetail(t, e, 5001) || hiddenDetail != 1 {
		t.Errorf("detail: shown %v, hidden rows %d; want both stored", hasDetail(t, e, 5001), hiddenDetail)
	}
	var pair []int64
	for _, id := range ev.ids {
		if id == 5001 || id == -777 {
			pair = append(pair, id)
		}
	}
	if !slices.Equal(pair, []int64{5001}) {
		t.Fatalf("events for the session %v, want only the shown copy 5001", pair)
	}
}
