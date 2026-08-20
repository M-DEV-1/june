package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LatestMemoryTime is the newest timestamp across episodes, threads, and working_state. Input: ctx. Output: the store's own "now" for recency, or zero if the store is empty. Evals use this instead of wall-clock so a snapshot scored the next day is not compared against the wrong today.
func (s *Store) LatestMemoryTime(ctx context.Context) (time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(t) FROM (
			SELECT MAX(created_at) AS t FROM episodes
			UNION ALL
			SELECT MAX(last_seen_at) AS t FROM threads
			UNION ALL
			SELECT MAX(updated_at) AS t FROM working_state
		)`).Scan(&raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("latest memory time: %w", err)
	}
	if !raw.Valid {
		return time.Time{}, nil
	}
	return parseSQLiteTime(raw.String), nil
}

// MemoryAsOf returns when a named source last happened. Input: source like "note:103", "thread:24", "episode:12", "episode:recent", or "working_state". Output: that row's created/last-seen/updated time, or zero if the source is missing or unknown.
func (s *Store) MemoryAsOf(ctx context.Context, source string) (time.Time, error) {
	kind, id, ok := splitMemorySource(source)
	if !ok {
		return time.Time{}, nil
	}
	var raw sql.NullString
	var err error
	switch kind {
	case "working_state":
		err = s.db.QueryRowContext(ctx, `SELECT updated_at FROM working_state WHERE id = 1`).Scan(&raw)
	case "note":
		err = s.db.QueryRowContext(ctx, `SELECT created_at FROM notes WHERE id = ?`, id).Scan(&raw)
	case "thread":
		err = s.db.QueryRowContext(ctx, `SELECT last_seen_at FROM threads WHERE id = ?`, id).Scan(&raw)
	case "episode":
		err = s.db.QueryRowContext(ctx, `SELECT created_at FROM episodes WHERE id = ?`, id).Scan(&raw)
	case "episode_recent":
		err = s.db.QueryRowContext(ctx, `SELECT created_at FROM episodes ORDER BY created_at DESC LIMIT 1`).Scan(&raw)
	default:
		return time.Time{}, nil
	}
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("memory as of %s: %w", source, err)
	}
	if !raw.Valid {
		return time.Time{}, nil
	}
	return parseSQLiteTime(raw.String), nil
}

func splitMemorySource(source string) (kind string, id int64, ok bool) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", 0, false
	}
	if source == "working_state" {
		return "working_state", 0, true
	}
	if source == "episode:recent" {
		return "episode_recent", 0, true
	}
	kind, rest, found := strings.Cut(source, ":")
	if !found || rest == "" {
		return "", 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	switch kind {
	case "note", "thread", "episode":
		return kind, id, true
	default:
		return "", 0, false
	}
}
