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
