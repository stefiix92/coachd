package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/fitdata"
)

// ActivityDetail is the laps, series and workout decoded from an activity's
// FIT file, computed once at store time. Series is already downsampled to
// fitdata.MaxSeriesPoints.
//
// The activity_detail table caches it as JSON of fitdata.Lap, fitdata.Point
// and fitdata.Workout, keyed by their Go field names (TestActivityDetailGoldenJSON
// pins the shape). Any change to fitdata's output (decoding, downsampling) or
// to those field names must ship with `DELETE FROM activity_detail`: the cache
// then rebuilds lazily on get_activity and from stored FITs on each FIT drain.
type ActivityDetail struct {
	Laps   []fitdata.Lap
	Series []fitdata.Point
	// Workout is the structured workout the activity recorded, or nil. It is
	// cached as JSON of fitdata.Workout in the same way, with its fingerprint
	// in a column of its own for ActivityWorkouts.
	Workout *fitdata.Workout
	// Perceived is written to the activity's own row (Activity.Perceived),
	// not to the cache; GetActivityDetail leaves it empty.
	Perceived fitdata.Perceived
}

// NewActivityDetail is the detail cached for a decoded activity: its laps,
// its series downsampled to fitdata.MaxSeriesPoints and its workout.
func NewActivityDetail(a fitdata.Activity) ActivityDetail {
	return ActivityDetail{Laps: a.Laps, Series: fitdata.Downsample(a.Points, fitdata.MaxSeriesPoints), Workout: a.Workout,
		Perceived: a.Perceived}
}

// PutActivityDetail upserts the detail for an activity. The activity row must exist.
func (db *DB) PutActivityDetail(ctx context.Context, id int64, d ActivityDetail) error {
	if d.Laps == nil {
		d.Laps = []fitdata.Lap{}
	}
	if d.Series == nil {
		d.Series = []fitdata.Point{}
	}
	laps, err := json.Marshal(d.Laps)
	if err != nil {
		return fmt.Errorf("store: encode laps: %w", err)
	}
	series, err := json.Marshal(d.Series)
	if err != nil {
		return fmt.Errorf("store: encode series: %w", err)
	}
	var wkt, fp sql.NullString
	if d.Workout != nil {
		b, err := json.Marshal(d.Workout)
		if err != nil {
			return fmt.Errorf("store: encode workout: %w", err)
		}
		wkt = sql.NullString{String: string(b), Valid: true}
		fp = sql.NullString{String: d.Workout.Fingerprint, Valid: true}
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: put activity detail: %w", err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `
INSERT INTO activity_detail (activity_id, laps_json, series_json, workout_json, workout_fp, computed_at) VALUES (?,?,?,?,?,?)
ON CONFLICT(activity_id) DO UPDATE SET laps_json=excluded.laps_json, series_json=excluded.series_json,
	workout_json=excluded.workout_json, workout_fp=excluded.workout_fp, computed_at=excluded.computed_at`,
		id, string(laps), string(series), wkt, fp, fmtTime(time.Now())); err != nil {
		return fmt.Errorf("store: put activity detail: %w", err)
	}
	var feel, rpe any
	if d.Perceived.FeelPct != nil {
		feel = *d.Perceived.FeelPct
	}
	if d.Perceived.RPE != nil {
		rpe = *d.Perceived.RPE
	}
	if _, err := tx.ExecContext(ctx, `UPDATE activities SET perceived_feel=?, perceived_rpe=? WHERE id=?`, feel, rpe, id); err != nil {
		return fmt.Errorf("store: put perceived: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: put activity detail: %w", err)
	}
	return nil
}

// GetActivityDetail returns the stored detail of a live activity, or
// ErrNotFound when absent or the activity is soft-deleted.
func (db *DB) GetActivityDetail(ctx context.Context, id int64) (ActivityDetail, error) {
	var laps, series string
	var wkt sql.NullString
	err := db.sql.QueryRowContext(ctx, `SELECT d.laps_json, d.series_json, d.workout_json FROM activity_detail d
		JOIN activities a ON a.id = d.activity_id WHERE d.activity_id=? AND `+liveActivityIn("a"), id).Scan(&laps, &series, &wkt)
	if errors.Is(err, sql.ErrNoRows) {
		return ActivityDetail{}, ErrNotFound
	}
	if err != nil {
		return ActivityDetail{}, fmt.Errorf("store: get activity detail: %w", err)
	}
	var d ActivityDetail
	if err := json.Unmarshal([]byte(laps), &d.Laps); err != nil {
		return ActivityDetail{}, fmt.Errorf("store: decode laps: %w", err)
	}
	if err := json.Unmarshal([]byte(series), &d.Series); err != nil {
		return ActivityDetail{}, fmt.Errorf("store: decode series: %w", err)
	}
	if wkt.Valid {
		if err := json.Unmarshal([]byte(wkt.String), &d.Workout); err != nil {
			return ActivityDetail{}, fmt.Errorf("store: decode workout: %w", err)
		}
	}
	return d, nil
}

// ActivityWorkout is a live activity that recorded a structured workout.
type ActivityWorkout struct {
	ID          int64
	Date        civil.Date
	Fingerprint string
}

// ActivityWorkouts returns the live activities dated in [from, to] whose
// cached detail holds a workout, oldest first (id ascending within a day).
func (db *DB) ActivityWorkouts(ctx context.Context, from, to civil.Date) ([]ActivityWorkout, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT a.id, a.date, d.workout_fp FROM activity_detail d
		JOIN activities a ON a.id = d.activity_id
		WHERE d.workout_fp IS NOT NULL AND a.date >= ? AND a.date <= ? AND `+liveActivityIn("a")+`
		ORDER BY a.date, a.id`, from.String(), to.String())
	if err != nil {
		return nil, fmt.Errorf("store: activity workouts: %w", err)
	}
	defer rows.Close()
	var out []ActivityWorkout
	for rows.Next() {
		var w ActivityWorkout
		var date string
		if err := rows.Scan(&w.ID, &date, &w.Fingerprint); err != nil {
			return nil, fmt.Errorf("store: activity workouts: %w", err)
		}
		if w.Date, err = civil.Parse(date); err != nil {
			return nil, fmt.Errorf("store: activity workouts: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// StoredFITsWithoutDetail returns live activities whose FIT is stored but that
// have no detail row, newest first.
func (db *DB) StoredFITsWithoutDetail(ctx context.Context, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	return db.queryActivities(ctx,
		`fit_status='stored' AND NOT EXISTS (SELECT 1 FROM activity_detail d WHERE d.activity_id = activities.id)`,
		nil, "ORDER BY date DESC, id DESC LIMIT "+strconv.Itoa(limit))
}
