package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stefiix92/coachd/internal/civil"
)

// Pain is one reported pain: where, which side, how bad (0-10).
type Pain struct {
	Area, Side string
	Level      int
}

// Checkin is the runner's check-in for one date. A scale left nil was not given.
type Checkin struct {
	Date                                                      civil.Date
	Soreness, Fatigue, Stress, Mood, Motivation, SleepQuality *int
	Pain                                                      []Pain
	Note                                                      string
	CreatedBy                                                 string // agent:<lane>:<client>
	UpdatedAt                                                 time.Time
}

// jsonList encodes a slice as JSON, an empty or nil one as [].
func jsonList[T any](v []T) (string, error) {
	if len(v) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func nilInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// UpsertCheckin replaces the date's check-in whole and writes one audit row
// (date and writer, no values), in one transaction.
func (db *DB) UpsertCheckin(ctx context.Context, c Checkin, audit AuditEvent) error {
	pain, err := jsonList(c.Pain)
	if err != nil {
		return fmt.Errorf("store: upsert checkin: %w", err)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: upsert checkin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkins (date, soreness, fatigue, stress, mood, motivation, sleep_quality, pain_json, note, created_by, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(date) DO UPDATE SET soreness=excluded.soreness, fatigue=excluded.fatigue, stress=excluded.stress, mood=excluded.mood,
			motivation=excluded.motivation, sleep_quality=excluded.sleep_quality, pain_json=excluded.pain_json, note=excluded.note,
			created_by=excluded.created_by, updated_at=excluded.updated_at`,
		c.Date.String(), nilInt(c.Soreness), nilInt(c.Fatigue), nilInt(c.Stress), nilInt(c.Mood), nilInt(c.Motivation), nilInt(c.SleepQuality),
		pain, c.Note, c.CreatedBy, fmtTime(c.UpdatedAt)); err != nil {
		return fmt.Errorf("store: upsert checkin: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit); err != nil {
		return fmt.Errorf("store: upsert checkin audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: upsert checkin: %w", err)
	}
	return nil
}

// ListCheckins returns the check-ins dated from..to inclusive, oldest first.
func (db *DB) ListCheckins(ctx context.Context, from, to civil.Date) ([]Checkin, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT date, soreness, fatigue, stress, mood, motivation, sleep_quality, pain_json, note, created_by, updated_at
		FROM checkins WHERE date >= ? AND date <= ? ORDER BY date`, from.String(), to.String())
	if err != nil {
		return nil, fmt.Errorf("store: list checkins: %w", err)
	}
	defer rows.Close()
	var out []Checkin
	for rows.Next() {
		var c Checkin
		var date, pain, updated string
		var so, fa, st, mo, mt, sq sql.NullInt64
		if err := rows.Scan(&date, &so, &fa, &st, &mo, &mt, &sq, &pain, &c.Note, &c.CreatedBy, &updated); err != nil {
			return nil, fmt.Errorf("store: list checkins: %w", err)
		}
		if c.Date, err = civil.Parse(date); err != nil {
			return nil, fmt.Errorf("store: list checkins: %w", err)
		}
		if c.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, fmt.Errorf("store: list checkins: %w", err)
		}
		if err := json.Unmarshal([]byte(pain), &c.Pain); err != nil {
			return nil, fmt.Errorf("store: list checkins: %w", err)
		}
		c.Soreness, c.Fatigue, c.Stress, c.Mood, c.Motivation, c.SleepQuality = intPtr(so), intPtr(fa), intPtr(st), intPtr(mo), intPtr(mt), intPtr(sq)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list checkins: %w", err)
	}
	return out, nil
}
