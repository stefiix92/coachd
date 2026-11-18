package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stefiix92/coachd/internal/fitdata"
)

func fp64(v float64) *float64 { return &v }

func TestActivityDetailRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	seedActivity(t, db, 7)
	want := ActivityDetail{
		Laps: []fitdata.Lap{
			{Index: 0, StartOffsetS: 0, DurationS: 60, DistanceKm: 0.2, AvgHRBpm: ip(140), AvgPaceSPerKm: fp64(300)},
			{Index: 1, StartOffsetS: 60, DurationS: 60, DistanceKm: 0.2}, // nil pointers
		},
		Series: []fitdata.Point{
			{OffsetS: 0, DistanceKm: fp64(0), HRBpm: ip(120), PaceSPerKm: fp64(310), AltitudeM: fp64(200)},
			{OffsetS: 1}, // all nil
		},
	}
	if _, err := db.GetActivityDetail(ctx, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent detail err = %v, want ErrNotFound", err)
	}
	if err := db.PutActivityDetail(ctx, 7, want); err != nil {
		t.Fatal(err)
	}
	if err := db.PutActivityDetail(ctx, 7, want); err != nil { // upsert
		t.Fatal(err)
	}
	got, err := db.GetActivityDetail(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if got.Laps[1].AvgHRBpm != nil || got.Series[1].HRBpm != nil {
		t.Fatal("nil pointer fields must stay nil")
	}
}

func TestActivityDetailCascadesOnDelete(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	seedActivity(t, db, 8)
	if err := db.PutActivityDetail(ctx, 8, ActivityDetail{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM activities WHERE id=8`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetActivityDetail(ctx, 8); !errors.Is(err, ErrNotFound) {
		t.Fatalf("detail after activity delete: err = %v, want ErrNotFound", err)
	}
}

func TestStoredFITsWithoutDetail(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	for _, id := range []int64{1, 2, 3} {
		seedActivity(t, db, id)
		if err := db.SetFIT(ctx, id, "fit/x.fit", FITStored); err != nil {
			t.Fatal(err)
		}
	}
	seedActivity(t, db, 4) // pending: excluded
	if err := db.PutActivityDetail(ctx, 2, ActivityDetail{}); err != nil {
		t.Fatal(err)
	}
	got, err := db.StoredFITsWithoutDetail(ctx, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d err %v, want 2", len(got), err)
	}
	if got, _ := db.StoredFITsWithoutDetail(ctx, 1); len(got) != 1 {
		t.Fatalf("limit ignored: %d", len(got))
	}
}

func seedActivity(t *testing.T, db *DB, id int64) {
	t.Helper()
	if err := db.UpsertActivity(context.Background(), mkAct(id, "2026-10-20", SportRunning, 10000), nil); err != nil {
		t.Fatal(err)
	}
}

// The cached JSON shape is pinned: fitdata.Lap and fitdata.Point have no JSON
// tags, so renaming a field changes the stored shape and old rows would decode
// with that field missing. See ActivityDetail.
func TestActivityDetailGoldenJSON(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	seedActivity(t, db, 9)
	d := ActivityDetail{
		Laps: []fitdata.Lap{{Index: 1, StartOffsetS: 60, DurationS: 300.5, DistanceKm: 1, AvgHRBpm: ip(150), MaxHRBpm: ip(171),
			AvgPaceSPerKm: fp64(300.5), ElevGainM: fp64(12.5), AvgPowerW: ip(200)}, {Index: 2}},
		Series: []fitdata.Point{{OffsetS: 1, DistanceKm: fp64(0.01), HRBpm: ip(120), PaceSPerKm: fp64(330), AltitudeM: fp64(201.5), PowerW: ip(185)}, {OffsetS: 2}},
	}
	const (
		laps = `[{"Index":1,"StartOffsetS":60,"DurationS":300.5,"DistanceKm":1,"AvgHRBpm":150,"MaxHRBpm":171,"AvgPaceSPerKm":300.5,"ElevGainM":12.5,"AvgPowerW":200},` +
			`{"Index":2,"StartOffsetS":0,"DurationS":0,"DistanceKm":0,"AvgHRBpm":null,"MaxHRBpm":null,"AvgPaceSPerKm":null,"ElevGainM":null,"AvgPowerW":null}]`
		series = `[{"OffsetS":1,"DistanceKm":0.01,"HRBpm":120,"PaceSPerKm":330,"AltitudeM":201.5,"PowerW":185},` +
			`{"OffsetS":2,"DistanceKm":null,"HRBpm":null,"PaceSPerKm":null,"AltitudeM":null,"PowerW":null}]`
	)
	if err := db.PutActivityDetail(ctx, 9, d); err != nil {
		t.Fatal(err)
	}
	var gotLaps, gotSeries string
	if err := db.sql.QueryRow(`SELECT laps_json, series_json FROM activity_detail WHERE activity_id=9`).Scan(&gotLaps, &gotSeries); err != nil {
		t.Fatal(err)
	}
	if gotLaps != laps {
		t.Errorf("laps_json =\n%s\nwant\n%s", gotLaps, laps)
	}
	if gotSeries != series {
		t.Errorf("series_json =\n%s\nwant\n%s", gotSeries, series)
	}
	// A row written in this shape (e.g. by an older binary) decodes back.
	if _, err := db.sql.Exec(`UPDATE activity_detail SET laps_json=?, series_json=? WHERE activity_id=9`, laps, series); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetActivityDetail(ctx, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("decoded golden row:\n got %+v\nwant %+v", got, d)
	}
}

// The cached workout JSON shape is pinned like the laps and series.
func TestActivityDetailWorkoutGoldenJSON(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	seedActivity(t, db, 9)
	d := ActivityDetail{Laps: []fitdata.Lap{}, Series: []fitdata.Point{}, Workout: &fitdata.Workout{
		NameUntrusted: "Easy",
		Steps: []fitdata.WorkoutStep{{Index: 0, Intensity: 2, DurationType: 0, DurationValue: 600000, TargetType: 1,
			TargetValue: 0, CustomLow: 210, CustomHigh: 238}},
		Executed: []fitdata.StepRun{{StepIndex: 0, Iteration: 1, LapCount: 2, StartOffsetS: 0, DurationS: 600, DistanceKm: 1.8,
			AvgHRBpm: ip(125), MaxHRBpm: ip(140), AvgPaceSPerKm: fp64(333.3), AvgPowerW: ip(205),
			TargetTime: &fitdata.TargetTime{MeasuredS: 600, InS: 400, BelowS: 100, AboveS: 100}}, {StepIndex: 1, Iteration: 1}},
		Fingerprint: "abc",
	}}
	const wkt = `{"NameUntrusted":"Easy","Steps":[{"Index":0,"Intensity":2,"DurationType":0,"DurationValue":600000,"TargetType":1,` +
		`"TargetValue":0,"CustomLow":210,"CustomHigh":238}],"Executed":[{"StepIndex":0,"Iteration":1,"LapCount":2,` +
		`"StartOffsetS":0,"DurationS":600,"DistanceKm":1.8,"AvgHRBpm":125,"MaxHRBpm":140,"AvgPaceSPerKm":333.3,"AvgPowerW":205,` +
		`"TargetTime":{"MeasuredS":600,"InS":400,"BelowS":100,"AboveS":100}},{"StepIndex":1,"Iteration":1,"LapCount":0,` +
		`"StartOffsetS":0,"DurationS":0,"DistanceKm":0,"AvgHRBpm":null,"MaxHRBpm":null,"AvgPaceSPerKm":null,"AvgPowerW":null,` +
		`"TargetTime":null}],"Fingerprint":"abc"}`
	if err := db.PutActivityDetail(ctx, 9, d); err != nil {
		t.Fatal(err)
	}
	var got, fp string
	if err := db.sql.QueryRow(`SELECT workout_json, workout_fp FROM activity_detail WHERE activity_id=9`).Scan(&got, &fp); err != nil {
		t.Fatal(err)
	}
	if got != wkt || fp != "abc" {
		t.Errorf("workout_json =\n%s\nwant\n%s\nworkout_fp = %q", got, wkt, fp)
	}
	back, err := db.GetActivityDetail(ctx, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, d) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", back.Workout, d.Workout)
	}
}

func TestActivityDetailWithoutWorkout(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	seedActivity(t, db, 9)
	if err := db.PutActivityDetail(ctx, 9, ActivityDetail{}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT count(*) FROM activity_detail WHERE workout_json IS NULL AND workout_fp IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("null workout columns: n=%d err=%v", n, err)
	}
	if d, err := db.GetActivityDetail(ctx, 9); err != nil || d.Workout != nil {
		t.Fatalf("detail = %+v, %v", d, err)
	}
}

func TestActivityWorkouts(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	put := func(id int64, date, fp string) {
		t.Helper()
		if err := db.UpsertActivity(ctx, mkAct(id, date, SportRunning, 5000), nil); err != nil {
			t.Fatal(err)
		}
		d := ActivityDetail{}
		if fp != "" {
			d.Workout = &fitdata.Workout{Fingerprint: fp}
		}
		if err := db.PutActivityDetail(ctx, id, d); err != nil {
			t.Fatal(err)
		}
	}
	put(1, "2026-10-20", "a")
	put(2, "2026-10-21", "")
	put(3, "2026-10-22", "b")
	put(4, "2026-10-20", "c")
	put(5, "2026-10-25", "a")
	if err := db.MarkActivityDeleted(ctx, 4, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := db.ActivityWorkouts(ctx, d("2026-10-20"), d("2026-10-22"))
	if err != nil {
		t.Fatal(err)
	}
	want := []ActivityWorkout{{1, d("2026-10-20"), "a"}, {3, d("2026-10-22"), "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestNewActivityDetailDownsamples(t *testing.T) {
	ps := make([]fitdata.Point, fitdata.MaxSeriesPoints*2)
	w := &fitdata.Workout{Fingerprint: "x"}
	d := NewActivityDetail(fitdata.Activity{Laps: []fitdata.Lap{{Index: 0}}, Points: ps, Workout: w})
	if len(d.Series) != fitdata.MaxSeriesPoints || len(d.Laps) != 1 || d.Workout != w {
		t.Fatalf("detail = %d points, %d laps, workout %v", len(d.Series), len(d.Laps), d.Workout)
	}
}

func TestPutActivityDetailWritesPerceived(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	seedActivity(t, db, 9)
	if err := db.PutActivityDetail(ctx, 9, ActivityDetail{Perceived: fitdata.Perceived{FeelPct: ip(75), RPE: fp64(6)}}); err != nil {
		t.Fatal(err)
	}
	a, err := db.GetActivity(ctx, 9)
	if err != nil || a.Perceived.FeelPct == nil || *a.Perceived.FeelPct != 75 || a.Perceived.RPE == nil || *a.Perceived.RPE != 6 {
		t.Fatalf("activity = %+v, %v", a.Perceived, err)
	}
	// Garmin listing the activity again leaves them; a detail without them clears them.
	if err := db.UpsertActivity(ctx, mkAct(9, "2026-10-20", SportRunning, 10000), nil); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.GetActivity(ctx, 9); a.Perceived.FeelPct == nil {
		t.Fatal("an upsert cleared the perceived rating")
	}
	if err := db.PutActivityDetail(ctx, 9, ActivityDetail{}); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.GetActivity(ctx, 9); a.Perceived.FeelPct != nil || a.Perceived.RPE != nil {
		t.Fatalf("perceived = %+v, want cleared", a.Perceived)
	}
}
