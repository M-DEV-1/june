package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"ora/internal/obs"
)

// DiaryDay is one kind='day' diary row: the local calendar day it covers and the entry Ora wrote for it.
type DiaryDay struct {
	Day     string
	Content string
}

// SetDiaryEntry upserts the diary row keyed by (day, kind), replacing whatever content was there before. Input: day as local 'YYYY-MM-DD' (empty for the single 'understanding' row), the kind, and the entry text. Output: an error when kind or content is blank or the write fails. The FTS mirror stays in sync via the diary_ai/diary_au triggers.
func (s *Store) SetDiaryEntry(ctx context.Context, day, kind, content string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.SetDiaryEntry")
	defer span.End()

	kind, content = strings.TrimSpace(kind), strings.TrimSpace(content)
	if kind == "" {
		return fmt.Errorf("diary entry needs a kind")
	}
	if content == "" {
		return fmt.Errorf("diary entry needs content")
	}

	if err := upsertDiary(ctx, s.db, day, kind, content); err != nil {
		span.RecordError(err)
		return err
	}
	return nil
}

// upsertDiary is the diary upsert against either the plain connection or a transaction — the dream stage commits need the diary write inside the same transaction as their stage token.
func upsertDiary(ctx context.Context, e execer, day, kind, content string) error {
	if _, err := e.ExecContext(ctx,
		`INSERT INTO diary (day, kind, content) VALUES (?, ?, ?)
		 ON CONFLICT(day, kind) DO UPDATE SET content = excluded.content, updated_at = CURRENT_TIMESTAMP`,
		day, kind, content); err != nil {
		return fmt.Errorf("set diary entry: %w", err)
	}
	return nil
}

// DiaryEntry returns the content of the diary row keyed by (day, kind), or "" when no such row exists — a missing entry is an ordinary state, not an error.
func (s *Store) DiaryEntry(ctx context.Context, day, kind string) (string, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.DiaryEntry")
	defer span.End()

	var content string
	err := s.db.QueryRowContext(ctx,
		`SELECT content FROM diary WHERE day = ? AND kind = ?`, day, kind).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		span.RecordError(err)
		return "", fmt.Errorf("get diary entry: %w", err)
	}
	return content, nil
}

// RecentDiaryEntries returns the newest n kind='day' diary rows, newest day first. Other kinds (the understanding doc, brief markers) never appear here.
func (s *Store) RecentDiaryEntries(ctx context.Context, n int) ([]DiaryDay, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.RecentDiaryEntries")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT day, content FROM diary WHERE kind = 'day' ORDER BY day DESC LIMIT ?`, n)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query recent diary entries: %w", err)
	}
	defer rows.Close()

	var out []DiaryDay
	for rows.Next() {
		var d DiaryDay
		if err := rows.Scan(&d.Day, &d.Content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan diary entry: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate diary entries: %w", err)
	}
	return out, nil
}

// DiaryDays returns the kind='day' diary rows whose day falls in [from, to], both local 'YYYY-MM-DD' strings, inclusive, oldest first. ISO date strings order lexically, so plain string comparison is the range check.
func (s *Store) DiaryDays(ctx context.Context, from, to string) ([]DiaryDay, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.DiaryDays")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT day, content FROM diary WHERE kind = 'day' AND day >= ? AND day <= ? ORDER BY day ASC`, from, to)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query diary days: %w", err)
	}
	defer rows.Close()

	var out []DiaryDay
	for rows.Next() {
		var d DiaryDay
		if err := rows.Scan(&d.Day, &d.Content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan diary day: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate diary days: %w", err)
	}
	return out, nil
}

// NotesOfKindSince returns notes of the given kind created at or after since, newest first. The proactive seams use it to pull the day's (or the last few days') meeting minutes without dragging in the whole notes table.
func (s *Store) NotesOfKindSince(ctx context.Context, kind string, since time.Time) ([]Note, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.NotesOfKindSince")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, kind, created_at, updated_at FROM notes WHERE kind = ? AND created_at >= ? ORDER BY id DESC`,
		kind, sqliteUTC(since))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query notes of kind since: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Content, &n.Kind, &n.CreatedAt, &n.UpdatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan note of kind: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate notes of kind: %w", err)
	}
	return out, nil
}
