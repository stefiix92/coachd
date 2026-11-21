package store

import (
	"context"
	"fmt"
	"time"
)

// MarkEvent records that event fired for key. first is true only for the call
// that inserted the row; any later call for the same event and key gets false.
func (db *DB) MarkEvent(ctx context.Context, event, key string, at time.Time) (first bool, err error) {
	res, err := db.sql.ExecContext(ctx, `INSERT INTO event_marks (event, key, at) VALUES (?,?,?) ON CONFLICT DO NOTHING`,
		event, key, fmtTime(at))
	if err != nil {
		return false, fmt.Errorf("store: mark event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: mark event: %w", err)
	}
	return n > 0, nil
}

// EventMark is one recorded event and its key.
type EventMark struct {
	Event, Key string
}

// EventMarks returns every recorded mark, by event then key. Tests use it to
// see what was marked.
func (db *DB) EventMarks(ctx context.Context) ([]EventMark, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT event, key FROM event_marks ORDER BY event, key`)
	if err != nil {
		return nil, fmt.Errorf("store: event marks: %w", err)
	}
	defer rows.Close()
	var out []EventMark
	for rows.Next() {
		var m EventMark
		if err := rows.Scan(&m.Event, &m.Key); err != nil {
			return nil, fmt.Errorf("store: event marks: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
