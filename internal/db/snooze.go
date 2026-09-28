// snooze.go is the storage behind "remind me later" on a notification: the user picks In an hour, This evening or Tomorrow, the notice is put away here with the moment it should come back, and the scheduler's per-minute tick posts it again when that moment arrives. Not memory: never indexed, never searched, never fed to a model.
package db

import (
	"context"
	"fmt"
	"time"
)

// Snooze is one notice put away until later. Kind and NoticeID are the notice's own kind and id, carried through so re-firing it can still act on what it was about; Title and Body are the text to post again; Due is when to post it.
type Snooze struct {
	ID       int64
	Kind     string
	NoticeID string
	Title    string
	Body     string
	Due      time.Time
}

// AddSnooze puts one notice away until due, first cancelling any snooze already pending for the same notice so a second snooze button replaces the first rather than stacking another one. Cancel and insert run in one transaction, so a failed insert leaves the earlier pending snooze untouched. Input: the notice's kind and id, the text to post again, and when to post it. Output: the new snooze's id.
func (s *Store) AddSnooze(ctx context.Context, kind, noticeID, title, body string, due time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add snooze: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful Commit

	if _, err := cancelSnoozes(ctx, tx, kind, noticeID); err != nil {
		return 0, fmt.Errorf("add snooze: %w", err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO snoozes (notice_kind, notice_id, title, body, due_at) VALUES (?, ?, ?, ?, ?)`, kind, noticeID, title, body, sqliteUTC(due))
	if err != nil {
		return 0, fmt.Errorf("add snooze: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add snooze: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("add snooze: commit: %w", err)
	}
	return id, nil
}

// CancelSnoozes marks every unfired snooze held for one notice as fired, so none of them can come back on their own — what markDone calls before reporting a notice cleared. Input: the notice's kind and id. Output: how many snoozes were cancelled.
func (s *Store) CancelSnoozes(ctx context.Context, kind, noticeID string) (int, error) {
	n, err := cancelSnoozes(ctx, s.db, kind, noticeID)
	if err != nil {
		return 0, fmt.Errorf("cancel snoozes: %w", err)
	}
	return n, nil
}

// cancelSnoozes is the shared statement behind CancelSnoozes and AddSnooze's own replace-on-add, run through ex so it can act standalone or inside AddSnooze's transaction. Input: the notice's kind and id. Output: how many unfired rows were stamped fired.
func cancelSnoozes(ctx context.Context, ex execer, kind, noticeID string) (int, error) {
	res, err := ex.ExecContext(ctx, `UPDATE snoozes SET fired_at = ? WHERE notice_kind = ? AND notice_id = ? AND fired_at IS NULL`, sqliteUTC(time.Now()), kind, noticeID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// DueSnoozes returns every snooze that has come due and has not been fired yet, oldest first. Input: the current time. Output: the snoozes to post again.
func (s *Store) DueSnoozes(ctx context.Context, now time.Time) ([]Snooze, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, notice_kind, notice_id, title, body, due_at FROM snoozes WHERE fired_at IS NULL AND due_at <= ? ORDER BY due_at`, sqliteUTC(now))
	if err != nil {
		return nil, fmt.Errorf("due snoozes: %w", err)
	}
	defer rows.Close()

	var out []Snooze
	for rows.Next() {
		var sn Snooze
		if err := rows.Scan(&sn.ID, &sn.Kind, &sn.NoticeID, &sn.Title, &sn.Body, &sn.Due); err != nil {
			return nil, fmt.Errorf("due snoozes: %w", err)
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// MarkSnoozeFired stamps a snooze as posted again, so it never comes back a second time. An id that matches nothing is an error, not a silent no-op.
func (s *Store) MarkSnoozeFired(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE snoozes SET fired_at = ? WHERE id = ? AND fired_at IS NULL`, sqliteUTC(time.Now()), id)
	if err != nil {
		return fmt.Errorf("mark snooze fired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark snooze fired: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no unfired snooze with id %d", id)
	}
	return nil
}
