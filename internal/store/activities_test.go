package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
)

func d(s string) civil.Date {
	v, err := civil.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func mkAct(id int64, date string, sport Sport, dist float64) Activity {
	dt := d(date)
	start := time.Date(dt.Year, dt.Month, dt.Day, 7, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	return Activity{
		ID: id, StartLocal: start, StartUTC: start.UTC(), Date: dt, WeekStart: dt.WeekStart(),
		TypeKey: "running", Sport: sport, NameUntrusted: "Morning Run",
		DistanceM: dist, DurationS: 3000, MovingS: 2900, ElevGainM: 50, ElevLossM: 48,
	}
}

func TestUpsertActivityIdempotent(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	a := mkAct(1, "2026-10-20", SportRunning, 5000)
	hr := 150.0
	a.AvgHR = &hr
	if err := db.UpsertActivity(ctx, a, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	a.DistanceM = 5100
	if err := db.UpsertActivity(ctx, a, []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	var n int
	db.sql.QueryRow(`SELECT count(*) FROM activities`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d", n)
	}
	got, err := db.GetActivity(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.DistanceM != 5100 || got.AvgHR == nil || *got.AvgHR != 150 || got.MaxHR != nil {
		t.Fatalf("got %+v", got)
	}
	if got.Date != a.Date || got.WeekStart != a.WeekStart || got.Sport != SportRunning || got.NameUntrusted != "Morning Run" {
		t.Fatalf("got %+v", got)
	}
	if !got.StartLocal.Equal(a.StartLocal) {
		t.Fatalf("start local %v != %v", got.StartLocal, a.StartLocal)
	}
	if _, off := got.StartLocal.Zone(); off != 2*3600 {
		t.Fatalf("offset lost: %d", off)
	}
	if got.FITStatus != FITPending {
		t.Fatalf("fit status %q", got.FITStatus)
	}
	var raw string
	db.sql.QueryRow(`SELECT raw_json FROM activities WHERE id=1`).Scan(&raw)
	if raw != `{"v":2}` {
		t.Fatalf("raw %q", raw)
	}
}

func TestGetActivityNotFound(t *testing.T) {
	db := openTest(t)
	if _, err := db.GetActivity(context.Background(), 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestUpsertActivityPreservesFIT(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	a := mkAct(1, "2026-10-20", SportRunning, 5000)
	if err := db.UpsertActivity(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFIT(ctx, 1, "fit/2026/1.fit", FITStored); err != nil {
		t.Fatal(err)
	}
	a.FITStatus = ""
	if err := db.UpsertActivity(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetActivity(ctx, 1)
	if got.FITStatus != FITStored || got.FITPath != "fit/2026/1.fit" {
		t.Fatalf("got %q %q", got.FITStatus, got.FITPath)
	}
}

func TestUpsertActivityPendingDoesNotResetStoredFIT(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	a := mkAct(1, "2026-10-20", SportRunning, 5000)
	db.UpsertActivity(ctx, a, nil)
	db.SetFIT(ctx, 1, "fit/2026/1.fit", FITStored)
	a.FITStatus = FITPending
	a.FITAttempts = 0
	if err := db.UpsertActivity(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetActivity(ctx, 1)
	if got.FITStatus != FITStored || got.FITPath != "fit/2026/1.fit" {
		t.Fatalf("got %q %q", got.FITStatus, got.FITPath)
	}
}

func TestListActivitiesPagination(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	for i := int64(1); i <= 5; i++ {
		db.UpsertActivity(ctx, mkAct(i, "2026-10-1"+string(rune('0'+i)), SportRunning, 1000), nil)
	}
	// same-day tie to exercise id desc ordering
	db.UpsertActivity(ctx, mkAct(6, "2026-10-15", SportCycling, 1000), nil)
	q := ActivityQuery{From: d("2026-10-01"), To: d("2026-10-31"), Limit: 2}
	var ids []int64
	pages := 0
	for {
		got, next, err := db.ListActivities(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		if next == "" {
			break
		}
		q.Cursor = next
	}
	want := []int64{6, 5, 4, 3, 2, 1}
	if pages != 3 || len(ids) != 6 {
		t.Fatalf("pages=%d ids=%v", pages, ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids=%v want %v", ids, want)
		}
	}
	// sport filter
	got, next, _ := db.ListActivities(ctx, ActivityQuery{From: d("2026-10-01"), To: d("2026-10-31"), Sport: SportCycling})
	if len(got) != 1 || next != "" {
		t.Fatalf("filter: %v %q", got, next)
	}
	if _, _, err := db.ListActivities(ctx, ActivityQuery{Cursor: "!!bad"}); err == nil {
		t.Fatal("want cursor error")
	}
}

func TestActivitiesNeedingFITStopsAfterThreeFailures(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	db.UpsertActivity(ctx, mkAct(1, "2026-10-20", SportRunning, 1), nil)
	db.UpsertActivity(ctx, mkAct(2, "2026-10-21", SportRunning, 1), nil)
	db.SetFIT(ctx, 2, "x.fit", FITStored)
	for i := 0; i < 2; i++ {
		db.SetFIT(ctx, 1, "", FITFailed)
		got, err := db.ActivitiesNeedingFIT(ctx, 10)
		if err != nil || len(got) != 1 || got[0].ID != 1 {
			t.Fatalf("after %d failures: %v %v", i+1, got, err)
		}
	}
	db.SetFIT(ctx, 1, "", FITFailed)
	got, _ := db.ActivitiesNeedingFIT(ctx, 10)
	if len(got) != 0 {
		t.Fatalf("still needing: %v", got)
	}
	a, _ := db.GetActivity(ctx, 1)
	if a.FITAttempts != 3 {
		t.Fatalf("attempts %d", a.FITAttempts)
	}
	if err := db.SetFIT(ctx, 99, "", FITStored); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

// HasFITWaiting sees live activities in the range whose FIT is pending, the
// hidden copy included; a stored, soft-deleted or capped-out one does not count.
func TestHasFITWaiting(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	pending := func(from, to string) bool {
		t.Helper()
		got, err := db.HasFITWaiting(ctx, d(from), d(to))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if pending("2026-11-02", "2026-11-03") {
		t.Fatal("pending on an empty store")
	}
	db.UpsertActivity(ctx, mkAct(1, "2026-11-02", SportRunning, 1), nil)
	db.UpsertActivity(ctx, mkAct(2, "2026-11-03", SportRunning, 1), nil)
	db.SetFIT(ctx, 2, "x.fit", FITStored)
	db.UpsertActivity(ctx, mkAct(3, "2026-11-03", SportRunning, 1), nil)
	for i := 0; i < 3; i++ {
		db.SetFIT(ctx, 3, "", FITFailed)
	}
	db.UpsertActivity(ctx, mkAct(4, "2026-11-03", SportRunning, 1), nil)
	db.MarkActivityDeleted(ctx, 4, time.Now())
	db.UpsertActivity(ctx, mkAct(5, "2026-10-31", SportRunning, 1), nil)
	if !pending("2026-11-02", "2026-11-03") || !pending("2026-10-31", "2026-10-31") {
		t.Fatal("a pending FIT in the range is not seen")
	}
	if pending("2026-11-03", "2026-11-03") || pending("2026-11-04", "2026-11-05") {
		t.Fatal("stored, failed, deleted or out-of-range activities count as pending")
	}
	db.SetFIT(ctx, 1, "y.fit", FITStored)
	if pending("2026-11-02", "2026-11-03") {
		t.Fatal("still pending after the FIT was stored")
	}
	db.UpsertActivity(ctx, mkAct(6, "2026-11-03", SportRunning, 1), nil)
	by := int64(2)
	if err := db.SetHiddenBy(ctx, 6, &by); err != nil {
		t.Fatal(err)
	}
	if !pending("2026-11-03", "2026-11-03") {
		t.Fatal("a hidden copy's pending FIT is not seen")
	}
}

func TestWeeklyVolumeGroupsBySportAndWeek(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	// 2026-10-19 is a Monday.
	db.UpsertActivity(ctx, mkAct(1, "2026-10-19", SportRunning, 5000), nil)
	db.UpsertActivity(ctx, mkAct(2, "2026-10-25", SportRunning, 7000), nil) // Sunday, same week
	db.UpsertActivity(ctx, mkAct(3, "2026-10-26", SportRunning, 3000), nil) // next week
	db.UpsertActivity(ctx, mkAct(4, "2026-10-20", SportCycling, 20000), nil)
	got, err := db.WeeklyVolume(ctx, d("2026-10-19"), d("2026-10-31"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	// ordered by week_start, sport
	if got[0].WeekStart != d("2026-10-19") || got[0].Sport != SportCycling || got[0].Count != 1 {
		t.Fatalf("%+v", got[0])
	}
	r := got[1]
	if r.Sport != SportRunning || r.DistanceM != 12000 || r.MovingS != 5800 || r.ElevGainM != 100 || r.Count != 2 {
		t.Fatalf("%+v", r)
	}
	if got[2].WeekStart != d("2026-10-26") || got[2].DistanceM != 3000 {
		t.Fatalf("%+v", got[2])
	}
}

func TestRunsFiltersSport(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	db.UpsertActivity(ctx, mkAct(1, "2026-10-19", SportRunning, 5000), nil)
	db.UpsertActivity(ctx, mkAct(2, "2026-10-20", SportTrailRunning, 5000), nil)
	db.UpsertActivity(ctx, mkAct(3, "2026-10-21", SportCycling, 5000), nil)
	got, err := db.Runs(ctx, d("2026-10-01"), d("2026-10-31"))
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
}

// rawActivity reads an activity row directly, soft-deleted or not.
func rawActivity(t *testing.T, db *DB, id int64) (deletedAt sql.NullString, raw []byte, fitPath string, details int) {
	t.Helper()
	if err := db.sql.QueryRow(`SELECT deleted_at, raw_json, fit_path FROM activities WHERE id=?`, id).
		Scan(&deletedAt, &raw, &fitPath); err != nil {
		t.Fatalf("activity %d row: %v", id, err)
	}
	if err := db.sql.QueryRow(`SELECT count(*) FROM activity_detail WHERE activity_id=?`, id).Scan(&details); err != nil {
		t.Fatal(err)
	}
	return deletedAt, raw, fitPath, details
}

func TestMarkActivityDeletedKeepsRawData(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	at := time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC)
	if err := db.UpsertActivity(ctx, mkAct(7, "2026-10-20", SportRunning, 5000), []byte(`{"activityId":7}`)); err != nil {
		t.Fatal(err)
	}
	seedActivity(t, db, 8)
	if err := db.SetFIT(ctx, 7, "fit/2026/7.fit", FITStored); err != nil {
		t.Fatal(err)
	}
	if err := db.PutActivityDetail(ctx, 7, ActivityDetail{}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActivityDeleted(ctx, 7, at); err != nil {
		t.Fatal(err)
	}
	del, raw, fitPath, details := rawActivity(t, db, 7)
	if !del.Valid || del.String != fmtTime(at) {
		t.Fatalf("deleted_at = %v, want %s", del, fmtTime(at))
	}
	if string(raw) != `{"activityId":7}` || fitPath != "fit/2026/7.fit" || details != 1 {
		t.Fatalf("raw %q fit %q details %d: raw JSON, FIT path and detail row must be kept", raw, fitPath, details)
	}
	if _, err := db.GetActivity(ctx, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetActivity after soft delete: err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetActivityDetail(ctx, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetActivityDetail after soft delete: err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetActivity(ctx, 8); err != nil {
		t.Fatalf("other activity: %v", err)
	}
	if err := db.MarkActivityDeleted(ctx, 7, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second mark: err = %v, want ErrNotFound", err)
	}
	if err := db.MarkActivityDeleted(ctx, 99, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
}

// A soft-deleted activity is invisible to every read and computation.
func TestSoftDeletedActivityExcludedFromReads(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	at := time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC)
	// 1: deleted run, the oldest stored date, FIT stored without detail.
	// 2: deleted run, FIT pending. 3: kept run. 4: kept ride.
	db.UpsertActivity(ctx, mkAct(1, "2026-10-12", SportRunning, 9000), nil)
	db.UpsertActivity(ctx, mkAct(2, "2026-10-20", SportRunning, 8000), nil)
	db.UpsertActivity(ctx, mkAct(3, "2026-10-20", SportRunning, 5000), nil)
	db.UpsertActivity(ctx, mkAct(4, "2026-10-21", SportCycling, 20000), nil)
	if err := db.SetFIT(ctx, 1, "fit/2026/1.fit", FITStored); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFIT(ctx, 3, "fit/2026/3.fit", FITStored); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := db.MarkActivityDeleted(ctx, id, at); err != nil {
			t.Fatal(err)
		}
	}
	from, to := d("2026-10-01"), d("2026-10-31")
	ids := func(as []Activity) []int64 {
		var out []int64
		for _, a := range as {
			out = append(out, a.ID)
		}
		return out
	}
	list, _, err := db.ListActivities(ctx, ActivityQuery{From: from, To: to})
	if err != nil || !slices.Equal(ids(list), []int64{4, 3}) {
		t.Fatalf("ListActivities = %v %v, want [4 3]", ids(list), err)
	}
	if all, _, err := db.ListActivities(ctx, ActivityQuery{}); err != nil || !slices.Equal(ids(all), []int64{4, 3}) {
		t.Fatalf("unbounded ListActivities = %v %v, want [4 3]", ids(all), err)
	}
	if runs, err := db.Runs(ctx, from, to); err != nil || !slices.Equal(ids(runs), []int64{3}) {
		t.Fatalf("Runs = %v %v, want [3]", ids(runs), err)
	}
	vols, err := db.WeeklyVolume(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		if v.Sport == SportRunning && (v.WeekStart != d("2026-10-19") || v.Count != 1 || v.DistanceM != 5000) {
			t.Fatalf("running volume %+v, want only activity 3", v)
		}
	}
	if len(vols) != 2 {
		t.Fatalf("WeeklyVolume = %+v, want running and cycling in one week", vols)
	}
	if got, err := db.ActivityIDsInRange(ctx, SourceGarmin, from, to); err != nil || !slices.Equal(got, []int64{3, 4}) {
		t.Fatalf("ActivityIDsInRange = %v %v, want [3 4]", got, err)
	}
	if got, err := db.ActivitiesNeedingFIT(ctx, 10); err != nil || !slices.Equal(ids(got), []int64{4}) {
		t.Fatalf("ActivitiesNeedingFIT = %v %v, want [4]", ids(got), err)
	}
	if got, err := db.StoredFITsWithoutDetail(ctx, 10); err != nil || !slices.Equal(ids(got), []int64{3}) {
		t.Fatalf("StoredFITsWithoutDetail = %v %v, want [3]", ids(got), err)
	}
	cov, err := db.Coverage(ctx, d("2026-10-23"))
	if err != nil || cov.FirstDate == nil || *cov.FirstDate != d("2026-10-20") {
		t.Fatalf("Coverage.FirstDate = %v %v, want 2026-10-20 (the deleted 2026-10-12 run excluded)", cov.FirstDate, err)
	}
	db.MarkActivityDeleted(ctx, 3, at)
	db.MarkActivityDeleted(ctx, 4, at)
	if cov, err := db.Coverage(ctx, d("2026-10-23")); err != nil || cov.FirstDate != nil {
		t.Fatalf("Coverage with only deleted activities = %+v %v, want empty", cov, err)
	}
}

// Garmin listing a soft-deleted activity again restores it, FIT state intact.
func TestUpsertRestoresSoftDeletedActivity(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	a := mkAct(7, "2026-10-20", SportRunning, 5000)
	if err := db.UpsertActivity(ctx, a, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFIT(ctx, 7, "fit/2026/7.fit", FITStored); err != nil {
		t.Fatal(err)
	}
	if err := db.PutActivityDetail(ctx, 7, ActivityDetail{}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActivityDeleted(ctx, 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertActivity(ctx, a, []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetActivity(ctx, 7)
	if err != nil {
		t.Fatalf("restored activity: %v", err)
	}
	if got.FITStatus != FITStored || got.FITPath != "fit/2026/7.fit" {
		t.Fatalf("restored FIT = %q %q, want stored fit/2026/7.fit", got.FITStatus, got.FITPath)
	}
	if del, raw, _, _ := rawActivity(t, db, 7); del.Valid || string(raw) != `{"v":2}` {
		t.Fatalf("deleted_at = %v raw = %q, want NULL and the new payload", del, raw)
	}
	if _, err := db.GetActivityDetail(ctx, 7); err != nil {
		t.Fatalf("restored detail: %v", err)
	}
}

func TestActivityIDsInRange(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	db.UpsertActivity(ctx, mkAct(4, "2026-10-08", SportRunning, 1), nil) // before
	db.UpsertActivity(ctx, mkAct(3, "2026-10-09", SportRunning, 1), nil) // first day
	db.UpsertActivity(ctx, mkAct(1, "2026-10-15", SportCycling, 1), nil)
	db.UpsertActivity(ctx, mkAct(2, "2026-10-15", SportRunning, 1), nil)
	db.UpsertActivity(ctx, mkAct(5, "2026-10-23", SportRunning, 1), nil) // last day
	db.UpsertActivity(ctx, mkAct(6, "2026-10-24", SportRunning, 1), nil) // after
	got, err := db.ActivityIDsInRange(ctx, SourceGarmin, d("2026-10-09"), d("2026-10-23"))
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{3, 1, 2, 5} // oldest first, id within a day
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
	if got, err := db.ActivityIDsInRange(ctx, SourceGarmin, d("2026-11-01"), d("2026-11-30")); err != nil || len(got) != 0 {
		t.Fatalf("empty range = %v, %v", got, err)
	}
}

func TestMaxHR(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	if v, err := db.MaxHR(ctx, d("2026-08-01"), d("2026-10-23")); err != nil || v != nil {
		t.Fatalf("empty = %v, %v", v, err)
	}
	hr := func(v float64) *float64 { return &v }
	a1 := mkAct(1, "2026-10-01", SportRunning, 5000)
	a1.MaxHR = hr(181)
	a2 := mkAct(2, "2026-10-10", SportRunning, 5000)
	a2.MaxHR = hr(190)
	a3 := mkAct(3, "2026-10-12", SportRunning, 5000) // no HR
	a4 := mkAct(4, "2026-07-01", SportRunning, 5000) // outside window
	a4.MaxHR = hr(210)
	a5 := mkAct(5, "2026-10-15", SportRunning, 5000) // soft-deleted
	a5.MaxHR = hr(200)
	for _, a := range []Activity{a1, a2, a3, a4, a5} {
		if err := db.UpsertActivity(ctx, a, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MarkActivityDeleted(ctx, 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	v, err := db.MaxHR(ctx, d("2026-07-26"), d("2026-10-23"))
	if err != nil || v == nil || *v != 190 {
		t.Fatalf("max = %v, %v", v, err)
	}
	if v, err := db.MaxHR(ctx, d("2026-10-11"), d("2026-10-14")); err != nil || v != nil {
		t.Fatalf("no-hr window = %v, %v", v, err)
	}
}

// TestMigrateSourceOnPopulatedDB builds a database at 0001-0015 holding two
// Garmin rows, one with average power in its raw JSON, then opens it: 0016
// applies, both rows stay shown Garmin rows and has_power follows the JSON.
func TestMigrateSourceOnPopulatedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coachd.db")
	ctx := context.Background()
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := old.Exec(q, args...); err != nil {
			t.Fatalf("%.40s: %v", q, err)
		}
	}
	exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY)`)
	for _, v := range migrationVersions() {
		if v >= "0016" {
			break
		}
		body, err := migrationFS.ReadFile("migrations/" + v + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		exec(`INSERT INTO schema_migrations(version) VALUES (?)`, v)
	}
	ins := `INSERT INTO activities (id, start_local, start_utc, date, week_start, sport, distance_m, raw_json)
		VALUES (?, '2026-10-20T07:30:00+02:00', '2026-10-20T05:30:00.000000000Z', '2026-10-20', '2026-10-19', 'cycling', 5000, ?)`
	exec(ins, 1, `{"activityId":1,"avgPower":187.0}`)
	exec(ins, 2, `{"activityId":2}`)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, wantPower := range map[int64]bool{1: true, 2: false} {
		a, err := db.GetActivity(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.Source != SourceGarmin || a.SourceID != "" || a.HiddenBy != nil || a.HasPower != wantPower {
			t.Fatalf("activity %d = source %q sourceID %q hiddenBy %v hasPower %v, want garmin, none, nil, %v",
				id, a.Source, a.SourceID, a.HiddenBy, a.HasPower, wantPower)
		}
	}
	got, _, err := db.ListActivities(ctx, ActivityQuery{})
	if err != nil || len(got) != 2 {
		t.Fatalf("ListActivities = %d rows, %v, want 2", len(got), err)
	}
}

// TestMigratePowerLoadOnPopulatedDB builds a database at 0001-0016 holding a
// Garmin ride with power, an intervals.icu ride with a FIT original and an
// intervals.icu GPX upload, then opens it: 0017 fills average power and
// intervals.icu's estimates from the raw JSON and queues the GPX upload's FIT.
func TestMigratePowerLoadOnPopulatedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coachd.db")
	ctx := context.Background()
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := old.Exec(q, args...); err != nil {
			t.Fatalf("%.40s: %v", q, err)
		}
	}
	exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY)`)
	for _, v := range migrationVersions() {
		if v >= "0017" {
			break
		}
		body, err := migrationFS.ReadFile("migrations/" + v + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		exec(`INSERT INTO schema_migrations(version) VALUES (?)`, v)
	}
	ins := `INSERT INTO activities (id, start_local, start_utc, date, week_start, sport, distance_m, source, source_id, fit_status, fit_attempts, raw_json)
		VALUES (?, '2026-10-20T07:30:00+02:00', '2026-10-20T05:30:00.000000000Z', '2026-10-20', '2026-10-19', 'cycling', 5000, ?, ?, ?, ?, ?)`
	exec(ins, 1, "garmin", nil, "stored", 0, `{"activityId":1,"avgPower":187.0}`)
	exec(ins, 2, "garmin", nil, "unavailable", 0, `{"activityId":2,"avgPower":0}`)
	exec(ins, -3, "intervals_icu", "i3", "stored", 0, `{"id":"i3","file_type":"fit","icu_average_watts":155,"icu_weighted_avg_watts":163,"icu_training_load":41,"icu_intensity":65.2}`)
	exec(ins, -4, "intervals_icu", "i4", "unavailable", 0, `{"id":"i4","file_type":"gpx","icu_average_watts":null,"icu_weighted_avg_watts":null,"icu_training_load":73,"icu_intensity":69.5}`)
	exec(ins, -5, "intervals_icu", "i5", "unavailable", 0, `{"id":"i5","source":"MANUAL"}`)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := func(v *float64) float64 {
		if v == nil {
			return -1
		}
		return *v
	}
	for _, w := range []struct {
		id             int64
		pw, tl, np, in float64 // -1: null
		fit            FITStatus
	}{
		{1, 187, -1, -1, -1, FITStored},
		{2, -1, -1, -1, -1, FITUnavailable}, // a Garmin row without a file is left alone
		{-3, 155, 41, 163, 0.652, FITStored},
		{-4, -1, 73, -1, 0.695, FITPending}, // the generated FIT is now worth a try
		{-5, -1, -1, -1, -1, FITUnavailable},
	} {
		a, err := db.GetActivity(ctx, w.id)
		if err != nil {
			t.Fatal(err)
		}
		iv := a.Intervals
		if f(a.AvgPowerW) != w.pw || f(iv.TrainingLoad) != w.tl || f(iv.NormalizedPowerW) != w.np ||
			math.Abs(f(iv.IntensityFactor)-w.in) > 1e-9 || a.FITStatus != w.fit {
			t.Errorf("activity %d = power %v, load %v, np %v, if %v, fit %s; want %+v", w.id,
				f(a.AvgPowerW), f(iv.TrainingLoad), f(iv.NormalizedPowerW), f(iv.IntensityFactor), a.FITStatus, w)
		}
	}
}

func TestUpsertIntervalsActivity(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	a := mkAct(-55751783, "2026-10-20", SportCycling, 40000)
	a.Source, a.SourceID, a.HasPower = SourceIntervals, "i55751783", true
	pw, tl, np, inf := 155.0, 41.0, 163.0, 0.652
	a.AvgPowerW, a.Intervals = &pw, ActivityIntervals{TrainingLoad: &tl, NormalizedPowerW: &np, IntensityFactor: &inf}
	for range 2 {
		if err := db.UpsertActivity(ctx, a, []byte(`{"id":"i55751783"}`)); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM activities`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v, want 1", n, err)
	}
	got, err := db.GetActivity(ctx, -55751783)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceIntervals || got.SourceID != "i55751783" || !got.HasPower || got.HiddenBy != nil {
		t.Fatalf("round trip = %q %q %v %v", got.Source, got.SourceID, got.HasPower, got.HiddenBy)
	}
	if iv := got.Intervals; got.AvgPowerW == nil || *got.AvgPowerW != 155 || iv.TrainingLoad == nil || *iv.TrainingLoad != 41 ||
		iv.NormalizedPowerW == nil || *iv.NormalizedPowerW != 163 || iv.IntensityFactor == nil || *iv.IntensityFactor != 0.652 {
		t.Fatalf("round trip power and intervals values = %v %+v", got.AvgPowerW, iv)
	}
	// An empty Source is a Garmin row with no source ID.
	if err := db.UpsertActivity(ctx, mkAct(9, "2026-10-20", SportRunning, 1), nil); err != nil {
		t.Fatal(err)
	}
	if g, err := db.GetActivity(ctx, 9); err != nil || g.Source != SourceGarmin || g.SourceID != "" {
		t.Fatalf("garmin default = %+v, %v", g, err)
	}
}

func TestHiddenActivityInvisible(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	a := mkAct(1, "2026-10-20", SportRunning, 5000)
	a.AvgHR, a.MaxHR = fp(150.0), fp(177.0)
	keep := mkAct(2, "2026-10-20", SportRunning, 1000)
	for _, x := range []Activity{a, keep} {
		if err := db.UpsertActivity(ctx, x, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PutActivityDetail(ctx, 1, ActivityDetail{}); err != nil {
		t.Fatal(err)
	}
	from, to := d("2026-10-20"), d("2026-10-20")
	two := int64(2)
	if err := db.SetHiddenBy(ctx, 1, &two); err != nil {
		t.Fatal(err)
	}
	if err := db.SetHiddenBy(ctx, 99, &two); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetHiddenBy absent = %v, want ErrNotFound", err)
	}
	check := func(visible bool) {
		t.Helper()
		_, getErr := db.GetActivity(ctx, 1)
		_, detErr := db.GetActivityDetail(ctx, 1)
		list, _, _ := db.ListActivities(ctx, ActivityQuery{})
		runs, _ := db.Runs(ctx, from, to)
		weeks, _ := db.WeeklyVolume(ctx, from, to)
		maxHR, _ := db.MaxHR(ctx, from, to)
		wantN, wantDist, wantHR := 1, 1000.0, false
		if visible {
			wantN, wantDist, wantHR = 2, 6000.0, true
		}
		var dist float64
		for _, w := range weeks {
			dist += w.DistanceM
		}
		if (getErr == nil) != visible || (detErr == nil) != visible || len(list) != wantN || len(runs) != wantN ||
			dist != wantDist || (maxHR != nil) != wantHR {
			t.Fatalf("visible=%v: get %v detail %v list %d runs %d dist %v maxHR %v", visible, getErr, detErr, len(list), len(runs), dist, maxHR)
		}
		if !visible && (!errors.Is(getErr, ErrNotFound) || !errors.Is(detErr, ErrNotFound)) {
			t.Fatalf("hidden get/detail = %v / %v, want ErrNotFound", getErr, detErr)
		}
	}
	check(false)
	if dup, err := db.ActivitiesForDuplicates(ctx, from, to); err != nil || len(dup) != 2 {
		t.Fatalf("ActivitiesForDuplicates = %d, %v, want 2 (hidden included)", len(dup), err)
	}
	if need, err := db.ActivitiesNeedingFIT(ctx, 10); err != nil || len(need) != 2 {
		t.Fatalf("ActivitiesNeedingFIT = %d, %v, want 2 (hidden copy's FIT is still downloaded)", len(need), err)
	}
	// Re-listing the hidden row must not show it again.
	if err := db.UpsertActivity(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := db.SetFIT(ctx, 1, "fit/2026/1.fit", FITStored); err != nil {
		t.Fatalf("SetFIT on hidden = %v", err)
	}
	if err := db.SetHiddenBy(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	check(true)
}

func TestActivityIDsInRangeBySource(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	g := mkAct(10, "2026-10-20", SportRunning, 1)
	i := mkAct(-10, "2026-10-20", SportRunning, 1)
	i.Source, i.SourceID = SourceIntervals, "i10"
	for _, a := range []Activity{g, i} {
		if err := db.UpsertActivity(ctx, a, nil); err != nil {
			t.Fatal(err)
		}
	}
	day := d("2026-10-20")
	if got, err := db.ActivityIDsInRange(ctx, SourceGarmin, day, day); err != nil || !slices.Equal(got, []int64{10}) {
		t.Fatalf("garmin ids = %v, %v", got, err)
	}
	if got, err := db.ActivityIDsInRange(ctx, SourceIntervals, day, day); err != nil || !slices.Equal(got, []int64{-10}) {
		t.Fatalf("intervals ids = %v, %v", got, err)
	}
}

func TestListActivitiesCursorNegativeID(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	for _, id := range []int64{-5, 7, -900} {
		if err := db.UpsertActivity(ctx, mkAct(id, "2026-10-20", SportRunning, 1), nil); err != nil {
			t.Fatal(err)
		}
	}
	var got []int64
	cursor := ""
	for range 5 {
		page, next, err := db.ListActivities(ctx, ActivityQuery{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range page {
			got = append(got, a.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.Equal(got, []int64{7, -5, -900}) {
		t.Fatalf("paged ids = %v, want [7 -5 -900]", got)
	}
}

// With a table alias the live condition qualifies every column it reads, so a
// join never relies on those names being unique across its tables.
func TestLiveActivityQualifiesEveryColumn(t *testing.T) {
	if got, want := liveActivityIn("a"), "a.deleted_at IS NULL AND a.hidden_by IS NULL"; got != want {
		t.Errorf(`liveActivityIn("a") = %q, want %q`, got, want)
	}
	if want := "deleted_at IS NULL AND hidden_by IS NULL"; liveActivity != want {
		t.Errorf("liveActivity = %q, want %q", liveActivity, want)
	}
}

// A failed FIT counts as waiting while attempts are left, the same rule as
// ActivitiesNeedingFIT, and stops counting at the cap.
func TestHasFITWaitingFailedWithAttemptsLeft(t *testing.T) {
	db, ctx := openTest(t), context.Background()
	db.UpsertActivity(ctx, mkAct(1, "2026-11-03", SportRunning, 1), nil)
	for i := 1; i <= 3; i++ {
		db.SetFIT(ctx, 1, "", FITFailed)
		got, err := db.HasFITWaiting(ctx, d("2026-11-03"), d("2026-11-03"))
		if err != nil {
			t.Fatal(err)
		}
		if want := i < 3; got != want {
			t.Fatalf("after %d failures: waiting = %v, want %v", i, got, want)
		}
	}
}
