package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/stefiix92/coachd/internal/civil"
)

const goalCols = `id, name, date, sport, distance_km, elevation_gain_m, target, notes, created_by, created_at, archived_at`

func scanGoal(s scanner) (Goal, error) {
	var g Goal
	var date, sport, created string
	var arch sql.NullString
	if err := s.Scan(&g.ID, &g.Name, &date, &sport, &g.DistanceKm, &g.ElevationGainM, &g.Target, &g.Notes, &g.CreatedBy, &created, &arch); err != nil {
		return g, err
	}
	var err error
	if g.Date, err = civil.Parse(date); err != nil {
		return g, err
	}
	g.Sport = Sport(sport)
	if g.CreatedAt, err = parseTime(created); err != nil {
		return g, err
	}
	g.ArchivedAt, err = parseTimePtr(arch)
	return g, err
}

// SetGoal archives the active goal (at g.CreatedAt) and inserts g as the new
// active one, in one transaction. It returns the stored goal.
func (db *DB) SetGoal(ctx context.Context, g Goal) (Goal, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return Goal{}, fmt.Errorf("store: set goal: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE goals SET archived_at=? WHERE archived_at IS NULL`, fmtTime(g.CreatedAt)); err != nil {
		return Goal{}, fmt.Errorf("store: archive goal: %w", err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO goals (name, date, sport, distance_km, elevation_gain_m, target, notes, created_by, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, g.Name, g.Date.String(), string(g.Sport), g.DistanceKm, g.ElevationGainM, g.Target, g.Notes, g.CreatedBy, fmtTime(g.CreatedAt))
	if err != nil {
		return Goal{}, fmt.Errorf("store: insert goal: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Goal{}, err
	}
	out, err := scanGoal(tx.QueryRowContext(ctx, `SELECT `+goalCols+` FROM goals WHERE id=?`, id))
	if err != nil {
		return Goal{}, fmt.Errorf("store: read goal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Goal{}, fmt.Errorf("store: set goal: %w", err)
	}
	return out, nil
}

// ActiveGoal returns the non-archived goal, or ErrNotFound.
func (db *DB) ActiveGoal(ctx context.Context) (Goal, error) {
	g, err := scanGoal(db.sql.QueryRowContext(ctx, `SELECT `+goalCols+` FROM goals WHERE archived_at IS NULL`))
	if errors.Is(err, sql.ErrNoRows) {
		return Goal{}, ErrNotFound
	}
	if err != nil {
		return Goal{}, fmt.Errorf("store: active goal: %w", err)
	}
	return g, nil
}

const noteCols = `id, category, text, created_by, created_at, updated_at, valid_until`

func scanNote(s scanner) (Note, error) {
	var n Note
	var cat, created, updated string
	var until sql.NullString
	if err := s.Scan(&n.ID, &cat, &n.Text, &n.CreatedBy, &created, &updated, &until); err != nil {
		return n, err
	}
	n.Category = NoteCategory(cat)
	var err error
	if until.Valid {
		d, err := civil.Parse(until.String)
		if err != nil {
			return n, err
		}
		n.ValidUntil = &d
	}
	if n.CreatedAt, err = parseTime(created); err != nil {
		return n, err
	}
	n.UpdatedAt, err = parseTime(updated)
	return n, err
}

// InsertNote inserts a new note (n.ID must be 0), returning ErrNotesFull when
// maxNotes is reached. The stored note is returned.
func (db *DB) InsertNote(ctx context.Context, n Note, maxNotes int) (Note, error) {
	if n.ID != 0 {
		return Note{}, errors.New("store: insert note: id must be 0")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, fmt.Errorf("store: insert note: %w", err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM notes`).Scan(&count); err != nil {
		return Note{}, fmt.Errorf("store: count notes: %w", err)
	}
	if count >= maxNotes {
		return Note{}, ErrNotesFull
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO notes (category, text, created_by, created_at, updated_at, valid_until) VALUES (?,?,?,?,?,?)`,
		string(n.Category), n.Text, n.CreatedBy, fmtTime(n.CreatedAt), fmtTime(n.UpdatedAt), dateOrNull(n.ValidUntil))
	if err != nil {
		return Note{}, fmt.Errorf("store: insert note: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Note{}, err
	}
	out, err := scanNote(tx.QueryRowContext(ctx, `SELECT `+noteCols+` FROM notes WHERE id=?`, id))
	if err != nil {
		return Note{}, fmt.Errorf("store: read note: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Note{}, fmt.Errorf("store: insert note: %w", err)
	}
	return out, nil
}

// agentAuthored matches notes whose created_by is an agent's, "agent:<lane>:<client>".
const agentAuthored = `created_by LIKE 'agent:%'`

// UpdateAgentNote sets category, text, valid_until, created_by and updated_at
// of a note an agent wrote; n.CreatedBy must be an agent's ("agent:..."), so an agent edit
// can never relabel a note as the runner's. It returns ErrNotFound when the
// note is absent and ErrNotOwned when someone other than an agent wrote it,
// decided in the update's own transaction.
func (db *DB) UpdateAgentNote(ctx context.Context, n Note) (Note, error) {
	if !strings.HasPrefix(n.CreatedBy, "agent:") {
		return Note{}, errors.New("store: update agent note: created_by lacks the agent: prefix")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, fmt.Errorf("store: update agent note: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE notes SET category=?, text=?, valid_until=?, created_by=?, updated_at=? WHERE id=? AND `+agentAuthored,
		string(n.Category), n.Text, dateOrNull(n.ValidUntil), n.CreatedBy, fmtTime(n.UpdatedAt), n.ID)
	if err != nil {
		return Note{}, fmt.Errorf("store: update agent note: %w", err)
	}
	if err := noteChanged(ctx, tx, res, n.ID); err != nil {
		return Note{}, err
	}
	out, err := scanNote(tx.QueryRowContext(ctx, `SELECT `+noteCols+` FROM notes WHERE id=?`, n.ID))
	if err != nil {
		return Note{}, fmt.Errorf("store: read note: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Note{}, fmt.Errorf("store: update agent note: %w", err)
	}
	return out, nil
}

// DeleteAgentNote removes a note an agent wrote. It returns ErrNotFound when
// the note is absent and ErrNotOwned when someone other than an agent wrote it.
func (db *DB) DeleteAgentNote(ctx context.Context, id int64) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete agent note: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM notes WHERE id=? AND `+agentAuthored, id)
	if err != nil {
		return fmt.Errorf("store: delete agent note: %w", err)
	}
	if err := noteChanged(ctx, tx, res, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete agent note: %w", err)
	}
	return nil
}

// noteChanged maps an agent-restricted write that changed no row to
// ErrNotFound or ErrNotOwned, reading in the write's transaction.
func noteChanged(ctx context.Context, tx *sql.Tx, res sql.Result, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM notes WHERE id=?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read note: %w", err)
	}
	return ErrNotOwned
}

// DeleteNote removes a note, or returns ErrNotFound.
func (db *DB) DeleteNote(ctx context.Context, id int64) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM notes WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: delete note: %w", err)
	}
	if c, err := res.RowsAffected(); err != nil {
		return err
	} else if c == 0 {
		return ErrNotFound
	}
	return nil
}

// ListNotes returns all notes ordered by category, then id.
func (db *DB) ListNotes(ctx context.Context) ([]Note, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+noteCols+` FROM notes ORDER BY category, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list notes: %w", err)
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan note: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// dateOrNull is d as a date column value: NULL when nil.
func dateOrNull(d *civil.Date) any {
	if d == nil {
		return nil
	}
	return d.String()
}
