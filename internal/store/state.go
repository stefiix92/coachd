package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const syncCols = `data_type, last_attempt_at, last_success_at, freshest_data_at, last_outcome, cursor, backfill_done`

func scanSyncState(s scanner) (SyncState, error) {
	var st SyncState
	var la, ls, lf sql.NullString
	var bf int
	if err := s.Scan(&st.DataType, &la, &ls, &lf, &st.LastOutcome, &st.Cursor, &bf); err != nil {
		return st, err
	}
	var err error
	if st.LastAttemptAt, err = parseTimePtr(la); err != nil {
		return st, err
	}
	if st.LastSuccessAt, err = parseTimePtr(ls); err != nil {
		return st, err
	}
	if st.FreshestDataAt, err = parseTimePtr(lf); err != nil {
		return st, err
	}
	st.BackfillDone = bf != 0
	return st, nil
}

// GetSyncState returns the zero value (not an error) when no state exists.
func (db *DB) GetSyncState(ctx context.Context, dataType string) (SyncState, error) {
	st, err := scanSyncState(db.sql.QueryRowContext(ctx, `SELECT `+syncCols+` FROM sync_state WHERE data_type=?`, dataType))
	if errors.Is(err, sql.ErrNoRows) {
		return SyncState{}, nil
	}
	if err != nil {
		return SyncState{}, fmt.Errorf("store: get sync state: %w", err)
	}
	return st, nil
}

func (db *DB) PutSyncState(ctx context.Context, s SyncState) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO sync_state (`+syncCols+`) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(data_type) DO UPDATE SET last_attempt_at=excluded.last_attempt_at,
		last_success_at=excluded.last_success_at, freshest_data_at=excluded.freshest_data_at,
		last_outcome=excluded.last_outcome, cursor=excluded.cursor, backfill_done=excluded.backfill_done`,
		s.DataType, fmtTimePtr(s.LastAttemptAt), fmtTimePtr(s.LastSuccessAt), fmtTimePtr(s.FreshestDataAt),
		s.LastOutcome, s.Cursor, boolInt(s.BackfillDone))
	if err != nil {
		return fmt.Errorf("store: put sync state: %w", err)
	}
	return nil
}

// DeleteSyncState removes dataType's state; an absent one is no error.
func (db *DB) DeleteSyncState(ctx context.Context, dataType string) error {
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM sync_state WHERE data_type=?`, dataType); err != nil {
		return fmt.Errorf("store: delete sync state: %w", err)
	}
	return nil
}

func (db *DB) ListSyncStates(ctx context.Context) ([]SyncState, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+syncCols+` FROM sync_state ORDER BY data_type`)
	if err != nil {
		return nil, fmt.Errorf("store: list sync states: %w", err)
	}
	defer rows.Close()
	var out []SyncState
	for rows.Next() {
		st, err := scanSyncState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
