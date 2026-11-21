package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// SportSettings is one sport-settings entry as the intervals.icu sync saw it.
// Units: watts, bpm, m/s; PowerZonesPct are percent of FTP and HRZonesBpm bpm,
// both upper bounds.
type SportSettings struct {
	Source, SourceID                           string // "intervals_icu", the entry's id
	Sports                                     []string
	FTPW, IndoorFTPW, ThresholdHRBpm, MaxHRBpm *int
	ThresholdSpeedMps                          *float64
	PowerZonesPct, HRZonesBpm                  []int
	UpdatedAt                                  *time.Time
}

// ReplaceSportSettings deletes source's rows and inserts ss, in one transaction.
func (db *DB) ReplaceSportSettings(ctx context.Context, source string, ss []SportSettings) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace sport settings: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sport_settings WHERE source=?`, source); err != nil {
		return fmt.Errorf("store: replace sport settings: %w", err)
	}
	for _, s := range ss {
		sports, err := jsonList(s.Sports)
		if err != nil {
			return fmt.Errorf("store: replace sport settings: %w", err)
		}
		pz, err := jsonList(s.PowerZonesPct)
		if err != nil {
			return fmt.Errorf("store: replace sport settings: %w", err)
		}
		hz, err := jsonList(s.HRZonesBpm)
		if err != nil {
			return fmt.Errorf("store: replace sport settings: %w", err)
		}
		var spd any
		if s.ThresholdSpeedMps != nil {
			spd = *s.ThresholdSpeedMps
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sport_settings (source, source_id, sports_json, ftp_w, indoor_ftp_w, threshold_hr_bpm, max_hr_bpm,
			threshold_speed_mps, power_zones_json, hr_zones_json, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			source, s.SourceID, sports, nilInt(s.FTPW), nilInt(s.IndoorFTPW), nilInt(s.ThresholdHRBpm), nilInt(s.MaxHRBpm),
			spd, pz, hz, fmtTimePtr(s.UpdatedAt)); err != nil {
			return fmt.Errorf("store: replace sport settings: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace sport settings: %w", err)
	}
	return nil
}

// ListSportSettings returns every entry, by source then source id.
func (db *DB) ListSportSettings(ctx context.Context) ([]SportSettings, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT source, source_id, sports_json, ftp_w, indoor_ftp_w, threshold_hr_bpm, max_hr_bpm,
		threshold_speed_mps, power_zones_json, hr_zones_json, updated_at FROM sport_settings ORDER BY source, source_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list sport settings: %w", err)
	}
	defer rows.Close()
	var out []SportSettings
	for rows.Next() {
		var s SportSettings
		var sports, pz, hz string
		var ftp, ift, thr, mx sql.NullInt64
		var spd sql.NullFloat64
		var upd sql.NullString
		if err := rows.Scan(&s.Source, &s.SourceID, &sports, &ftp, &ift, &thr, &mx, &spd, &pz, &hz, &upd); err != nil {
			return nil, fmt.Errorf("store: list sport settings: %w", err)
		}
		for _, d := range []struct {
			src string
			dst any
		}{{sports, &s.Sports}, {pz, &s.PowerZonesPct}, {hz, &s.HRZonesBpm}} {
			if err := json.Unmarshal([]byte(d.src), d.dst); err != nil {
				return nil, fmt.Errorf("store: list sport settings: %w", err)
			}
		}
		s.FTPW, s.IndoorFTPW, s.ThresholdHRBpm, s.MaxHRBpm = intPtr(ftp), intPtr(ift), intPtr(thr), intPtr(mx)
		if spd.Valid {
			v := spd.Float64
			s.ThresholdSpeedMps = &v
		}
		if s.UpdatedAt, err = parseTimePtr(upd); err != nil {
			return nil, fmt.Errorf("store: list sport settings: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sport settings: %w", err)
	}
	return out, nil
}
