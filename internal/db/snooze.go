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

// AddSnooze puts one notice away until due. Input: the notice's kind and id, the text to post again, and when to post it. Output: the new snooze's id.
func (s *Store) AddSnooze(ctx context.Context, kind, noticeID, title, body string, due time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO snoozes (notice_kind, notice_id, title, body, due_at) VALUES (?, ?, ?, ?, ?)`, kind, noticeID, title, body, sqliteUTC(due))
	if err != nil {
		return 0, fmt.Errorf("add snooze: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add snooze: %w", err)
	}
	return id, nil
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
