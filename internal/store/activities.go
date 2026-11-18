package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
	"github.com/stefiix92/coachd/internal/fitdata"
)

const activityCols = `id, start_local, start_utc, date, week_start, type_key, sport, name_untrusted,
	distance_m, duration_s, moving_s, elev_gain_m, elev_loss_m, avg_hr, max_hr, manual,
	training_load, aerobic_te, anaerobic_te, vo2max, tss, fit_path, fit_status, fit_attempts,
	source, source_id, has_power`

// activityReadCols are activityCols plus the columns only the detail writes.
const activityReadCols = activityCols + `, perceived_feel, perceived_rpe, hidden_by`

const defaultListLimit = 50

// UpsertActivity inserts or updates an activity keyed by activity ID (negative for intervals.icu). It never writes hidden_by.
// FIT fields are written only on insert (empty FITStatus becomes "pending");
// on conflict they are left untouched, only SetFIT changes FIT state. Garmin
// listing an activity again restores it if it was soft-deleted.
func (db *DB) UpsertActivity(ctx context.Context, a Activity, raw []byte) error {
	status := a.FITStatus
	if status == "" {
		status = FITPending
	}
	src := a.Source
	if src == "" {
		src = SourceGarmin
	}
	var sourceID any
	if a.SourceID != "" {
		sourceID = a.SourceID
	}
	_, err := db.sql.ExecContext(ctx, `
INSERT INTO activities (`+activityCols+`, raw_json)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
	start_local=excluded.start_local, start_utc=excluded.start_utc, date=excluded.date,
	week_start=excluded.week_start, type_key=excluded.type_key, sport=excluded.sport,
	name_untrusted=excluded.name_untrusted, distance_m=excluded.distance_m,
	duration_s=excluded.duration_s, moving_s=excluded.moving_s, elev_gain_m=excluded.elev_gain_m,
	elev_loss_m=excluded.elev_loss_m, avg_hr=excluded.avg_hr, max_hr=excluded.max_hr,
	manual=excluded.manual, training_load=excluded.training_load, aerobic_te=excluded.aerobic_te,
	anaerobic_te=excluded.anaerobic_te, vo2max=excluded.vo2max, tss=excluded.tss,
	source=excluded.source, source_id=excluded.source_id, has_power=excluded.has_power,
	raw_json=excluded.raw_json, deleted_at=NULL`,
		a.ID, a.StartLocal.Format(time.RFC3339Nano), fmtTime(a.StartUTC), a.Date.String(), a.WeekStart.String(),
		a.TypeKey, string(a.Sport), a.NameUntrusted,
		a.DistanceM, a.DurationS, a.MovingS, a.ElevGainM, a.ElevLossM, a.AvgHR, a.MaxHR, boolInt(a.Manual),
		a.Garmin.TrainingLoad, a.Garmin.AerobicTE, a.Garmin.AnaerobicTE, a.Garmin.VO2Max, a.Garmin.TSS,
		a.FITPath, string(status), a.FITAttempts, string(src), sourceID, boolInt(a.HasPower), raw)
	if err != nil {
		return fmt.Errorf("store: upsert activity: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type scanner interface{ Scan(dest ...any) error }

func scanActivity(s scanner) (Activity, error) {
	var (
		a                       Activity
		startLocal, startUTC    string
		date, week, sport, fits string
		manual                  int
		avgHR, maxHR            sql.NullFloat64
		tl, ate, nte, vo2, tss  sql.NullFloat64
		feel                    sql.NullInt64
		rpe                     sql.NullFloat64
		source                  string
		sourceID                sql.NullString
		hasPower                int
		hiddenBy                sql.NullInt64
	)
	if err := s.Scan(&a.ID, &startLocal, &startUTC, &date, &week, &a.TypeKey, &sport, &a.NameUntrusted,
		&a.DistanceM, &a.DurationS, &a.MovingS, &a.ElevGainM, &a.ElevLossM, &avgHR, &maxHR, &manual,
		&tl, &ate, &nte, &vo2, &tss, &a.FITPath, &fits, &a.FITAttempts,
		&source, &sourceID, &hasPower, &feel, &rpe, &hiddenBy); err != nil {
		return a, err
	}
	a.Source, a.HasPower = Source(source), hasPower != 0
	if sourceID.Valid {
		a.SourceID = sourceID.String
	}
	if hiddenBy.Valid {
		a.HiddenBy = &hiddenBy.Int64
	}
	a.Perceived = fitdata.Perceived{FeelPct: iptr(feel), RPE: fptr(rpe)}
	var err error
	if a.StartLocal, err = time.Parse(time.RFC3339Nano, startLocal); err != nil {
		return a, err
	}
	if a.StartUTC, err = parseTime(startUTC); err != nil {
		return a, err
	}
	if a.Date, err = civil.Parse(date); err != nil {
		return a, err
	}
	if a.WeekStart, err = civil.Parse(week); err != nil {
		return a, err
	}
	a.Sport, a.FITStatus, a.Manual = Sport(sport), FITStatus(fits), manual != 0
	a.AvgHR, a.MaxHR = fptr(avgHR), fptr(maxHR)
	a.Garmin = ActivityGarmin{TrainingLoad: fptr(tl), AerobicTE: fptr(ate), AnaerobicTE: fptr(nte), VO2Max: fptr(vo2), TSS: fptr(tss)}
	return a, nil
}

func fptr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

func iptr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func sptr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

// liveActivity is the condition every read of activities applies: rows
// soft-deleted by MarkActivityDeleted, and the poorer copy of a session both
// sources recorded (hidden_by set), are invisible.
const liveActivity = `deleted_at IS NULL AND hidden_by IS NULL`

// GetActivity returns a live activity, or ErrNotFound when absent or soft-deleted.
func (db *DB) GetActivity(ctx context.Context, id int64) (Activity, error) {
	a, err := scanActivity(db.sql.QueryRowContext(ctx,
		`SELECT `+activityReadCols+` FROM activities WHERE id=? AND `+liveActivity, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Activity{}, ErrNotFound
	}
	if err != nil {
		return Activity{}, fmt.Errorf("store: get activity: %w", err)
	}
	return a, nil
}

// queryActivities selects live activities matching cond (a WHERE condition
// without the keyword; empty for all), followed by tail.
func (db *DB) queryActivities(ctx context.Context, cond string, args []any, tail string) ([]Activity, error) {
	where := liveActivity
	if cond != "" {
		where += ` AND (` + cond + `)`
	}
	return db.queryActivitiesWhere(ctx, where, args, tail)
}

// queryActivitiesWhere selects activities matching where, which carries its
// own visibility condition.
func (db *DB) queryActivitiesWhere(ctx context.Context, where string, args []any, tail string) ([]Activity, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+activityReadCols+` FROM activities WHERE `+where+` `+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query activities: %w", err)
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan activity: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func isZero(d civil.Date) bool { return d == civil.Date{} }

// ListActivities pages activities by date desc, id desc. A zero From or To is unbounded.
func (db *DB) ListActivities(ctx context.Context, q ActivityQuery) ([]Activity, string, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	var conds []string
	var args []any
	if !isZero(q.From) {
		conds = append(conds, "date >= ?")
		args = append(args, q.From.String())
	}
	if !isZero(q.To) {
		conds = append(conds, "date <= ?")
		args = append(args, q.To.String())
	}
	if q.Sport != "" {
		conds = append(conds, "sport = ?")
		args = append(args, string(q.Sport))
	}
	if q.Cursor != "" {
		cd, cid, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "(date < ? OR (date = ? AND id < ?))")
		args = append(args, cd, cd, cid)
	}
	got, err := db.queryActivities(ctx, strings.Join(conds, " AND "), args, "ORDER BY date DESC, id DESC LIMIT "+strconv.Itoa(limit+1))
	if err != nil {
		return nil, "", err
	}
	if len(got) <= limit {
		return got, "", nil
	}
	got = got[:limit]
	last := got[limit-1]
	return got, encodeCursor(last.Date.String(), last.ID), nil
}

func encodeCursor(date string, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(date + "|" + strconv.FormatInt(id, 10)))
}

func decodeCursor(c string) (string, int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", 0, errors.New("store: invalid cursor")
	}
	date, idStr, ok := strings.Cut(string(b), "|")
	if !ok {
		return "", 0, errors.New("store: invalid cursor")
	}
	if _, err := civil.Parse(date); err != nil {
		return "", 0, errors.New("store: invalid cursor")
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "", 0, errors.New("store: invalid cursor")
	}
	return date, id, nil
}

// ActivitiesNeedingFIT returns activities whose FIT is pending, or failed fewer
// than 3 times. Hidden copies are included: their FIT is still downloaded.
func (db *DB) ActivitiesNeedingFIT(ctx context.Context, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	return db.queryActivitiesWhere(ctx, `deleted_at IS NULL AND (fit_status='pending' OR (fit_status='failed' AND fit_attempts < 3))`, nil,
		"ORDER BY date DESC, id DESC LIMIT "+strconv.Itoa(limit))
}

// SetFIT records FIT state; status failed increments fit_attempts. It also
// updates a soft-deleted row, so a download in flight is not orphaned.
func (db *DB) SetFIT(ctx context.Context, id int64, path string, status FITStatus) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE activities SET fit_path=?, fit_status=?, fit_attempts = fit_attempts + ? WHERE id=?`,
		path, string(status), boolInt(status == FITFailed), id)
	if err != nil {
		return fmt.Errorf("store: set fit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ActivityIDsInRange returns the IDs of src's live activities dated in
// [from, to], oldest first (id ascending within a day).
func (db *DB) ActivityIDsInRange(ctx context.Context, src Source, from, to civil.Date) ([]int64, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id FROM activities WHERE source=? AND date >= ? AND date <= ? AND `+liveActivity+` ORDER BY date, id`,
		string(src), from.String(), to.String())
	if err != nil {
		return nil, fmt.Errorf("store: activity ids: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: activity ids: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ActivitiesForDuplicates returns the activities dated in [from, to] that are
// not soft-deleted, hidden ones included, ordered by start_utc, id.
func (db *DB) ActivitiesForDuplicates(ctx context.Context, from, to civil.Date) ([]Activity, error) {
	return db.queryActivitiesWhere(ctx, `deleted_at IS NULL AND date >= ? AND date <= ?`,
		[]any{from.String(), to.String()}, "ORDER BY start_utc, id")
}

// SetHiddenBy hides the activity behind the richer copy by, or shows it again
// when by is nil. It returns ErrNotFound when the row is absent.
func (db *DB) SetHiddenBy(ctx context.Context, id int64, by *int64) error {
	var v any
	if by != nil {
		v = *by
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE activities SET hidden_by=? WHERE id=?`, v, id)
	if err != nil {
		return fmt.Errorf("store: set hidden by: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkActivityDeleted soft-deletes a live activity at at: every read then
// skips it, but the row, its raw JSON, FIT state, detail row and FIT file are
// all kept. It returns ErrNotFound when the activity is absent or already
// soft-deleted.
func (db *DB) MarkActivityDeleted(ctx context.Context, id int64, at time.Time) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE activities SET deleted_at=? WHERE id=? AND `+liveActivity, fmtTime(at), id)
	if err != nil {
		return fmt.Errorf("store: mark activity deleted: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Runs returns running and trail running activities in [from, to], oldest first.
func (db *DB) Runs(ctx context.Context, from, to civil.Date) ([]Activity, error) {
	return db.queryActivities(ctx, `sport IN ('running','trail_running') AND date >= ? AND date <= ?`,
		[]any{from.String(), to.String()}, "ORDER BY date, id")
}

// WeeklyVolume sums live activities per Monday-start week and sport, for dates in [from, to].
func (db *DB) WeeklyVolume(ctx context.Context, from, to civil.Date) ([]WeekVolume, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT week_start, sport, sum(distance_m), sum(moving_s), sum(elev_gain_m), count(*)
FROM activities WHERE date >= ? AND date <= ? AND `+liveActivity+`
GROUP BY week_start, sport ORDER BY week_start, sport`, from.String(), to.String())
	if err != nil {
		return nil, fmt.Errorf("store: weekly volume: %w", err)
	}
	defer rows.Close()
	var out []WeekVolume
	for rows.Next() {
		var w WeekVolume
		var ws, sport string
		if err := rows.Scan(&ws, &sport, &w.DistanceM, &w.MovingS, &w.ElevGainM, &w.Count); err != nil {
			return nil, err
		}
		if w.WeekStart, err = civil.Parse(ws); err != nil {
			return nil, err
		}
		w.Sport = Sport(sport)
		out = append(out, w)
	}
	return out, rows.Err()
}

// MaxHR returns the highest max_hr over live activities dated in [from, to],
// or nil when none has a value.
func (db *DB) MaxHR(ctx context.Context, from, to civil.Date) (*float64, error) {
	var v sql.NullFloat64
	err := db.sql.QueryRowContext(ctx, `SELECT max(max_hr) FROM activities WHERE date >= ? AND date <= ? AND `+liveActivity,
		from.String(), to.String()).Scan(&v)
	if err != nil {
		return nil, fmt.Errorf("store: max hr: %w", err)
	}
	return fptr(v), nil
}
