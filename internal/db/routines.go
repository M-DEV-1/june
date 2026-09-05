// This file is the storage behind user-authored routines: scheduled instructions Ora checks on its own, such as "every weekday at 8, tell me the one thing I must do today" or "when Priya replies about the venue, tell me". Schedule is kept as the free text the user typed — internal/proactive is what parses it into when to check. Not memory: never indexed, never searched, never fed to a model as context; a routine's own text goes into a prompt only when internal/proactive or a run-now request builds one to run it.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// RoutineNothing is the exact word a routine's answer must equal for callers to skip sending a notice, since there was nothing worth saying. Shared by internal/ipc's run-now handler and internal/proactive's scheduled runner, which must treat the same word the same way.
const RoutineNothing = "NOTHING"

// RoutineSuffix is appended to a routine's own instruction before it is asked, so the model answers in a shape both a notice and GET /routines can use. Shared by internal/ipc and internal/proactive.
const RoutineSuffix = "\n\nAnswer in one or two sentences, with the reason, or reply exactly NOTHING if there is nothing worth saying."

// Routine is one user-authored scheduled instruction. LastRun is the zero time until it has fired at least once; LastAnswer is what the model answered the last time it ran, "NOTHING" included.
type Routine struct {
	ID         int64
	Text       string
	Schedule   string
	Enabled    bool
	LastRun    time.Time
	LastAnswer string
	Created    time.Time
}

// AddRoutine stores a new routine, enabled from the moment it is created. Input: the instruction and its schedule, both required. Output: the new routine's id.
func (s *Store) AddRoutine(ctx context.Context, text, schedule string) (int64, error) {
	text, schedule = strings.TrimSpace(text), strings.TrimSpace(schedule)
	if text == "" || schedule == "" {
		return 0, fmt.Errorf("a routine needs both an instruction and a schedule")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO routines (text, schedule) VALUES (?, ?)`, text, schedule)
	if err != nil {
		return 0, fmt.Errorf("add routine: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add routine: %w", err)
	}
	return id, nil
}

// Routines returns every routine, newest first.
func (s *Store) Routines(ctx context.Context) ([]Routine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text, schedule, enabled, last_run, last_answer, created_at FROM routines ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("routines: %w", err)
	}
	defer rows.Close()

	var out []Routine
	for rows.Next() {
		r, err := scanRoutine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoutineByID reads one routine. An id that matches nothing is an error.
func (s *Store) RoutineByID(ctx context.Context, id int64) (Routine, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, text, schedule, enabled, last_run, last_answer, created_at FROM routines WHERE id = ?`, id)
	r, err := scanRoutine(row)
	if err == sql.ErrNoRows {
		return Routine{}, fmt.Errorf("no routine with id %d", id)
	}
	if err != nil {
		return Routine{}, fmt.Errorf("routine by id: %w", err)
	}
	return r, nil
}

// rowScanner is the sql.Rows/sql.Row method scanRoutine needs, so one scan body serves both a list read and a single-row read.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRoutine reads one routines row, translating a NULL last_run (a routine that has never fired) to the zero time.
func scanRoutine(row rowScanner) (Routine, error) {
	var r Routine
	var lastRun sql.NullTime
	if err := row.Scan(&r.ID, &r.Text, &r.Schedule, &r.Enabled, &lastRun, &r.LastAnswer, &r.Created); err != nil {
		return Routine{}, err
	}
	if lastRun.Valid {
		r.LastRun = lastRun.Time
	}
	return r, nil
}

// DeleteRoutine removes a routine. An id that matches nothing is an error, not a silent no-op.
func (s *Store) DeleteRoutine(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM routines WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete routine: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete routine: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no routine with id %d", id)
	}
	return nil
}

// TryStart marks routine id as running, unless it already is. Output: true if this call is the one that gets to run it, meaning the caller must call Finish(id) once it is done; false if another run of the same routine is already in flight, meaning the caller must skip this one rather than ask and write its result a second time. This is what stops the scheduler's own tick and a POST /routines/{id}/run from both running the same routine at once — before this existed, nothing shared between the two paths noticed the overlap.
func (s *Store) TryStart(id int64) bool {
	s.routineMu.Lock()
	defer s.routineMu.Unlock()
	if s.routineRunning == nil {
		s.routineRunning = make(map[int64]bool)
	}
	if s.routineRunning[id] {
		return false
	}
	s.routineRunning[id] = true
	return true
}

// Finish clears routine id's in-flight mark, set by a prior TryStart(id) that returned true. Safe to call even if id was never marked running.
func (s *Store) Finish(id int64) {
	s.routineMu.Lock()
	defer s.routineMu.Unlock()
	delete(s.routineRunning, id)
}

// SetRoutineRun records the result of running a routine once: when it ran and what the model answered, "NOTHING" included. An id that matches nothing is an error.
func (s *Store) SetRoutineRun(ctx context.Context, id int64, when time.Time, answer string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE routines SET last_run = ?, last_answer = ? WHERE id = ?`, sqliteUTC(when), answer, id)
	if err != nil {
		return fmt.Errorf("set routine run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set routine run: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no routine with id %d", id)
	}
	return nil
}
