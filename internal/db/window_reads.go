// This file holds the reads the desktop window's screens need and nothing else in the store already answers.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LatestDigest returns the content of the most recently written daily digest node. Input: none. Output: the digest prose, or "" when no digest has ever been written — a store too young to have compacted anything is an ordinary state, not an error.
func (s *Store) LatestDigest(ctx context.Context) (string, error) {
	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM nodes WHERE type = 'digest' ORDER BY id DESC LIMIT 1`).Scan(&content)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("latest digest: %w", err)
	}
	return content, nil
}

// MentionCount returns how many notes name someone. Input: a person's name. Output: the number of notes (facts, meeting minutes and action items alike) whose FTS row matches that name as a phrase, or 0 for a blank name. Only notes are counted: summaries and threads are the compiler's own prose about what happened, not the record of who was there.
func (s *Store) MentionCount(ctx context.Context, name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_fts WHERE memory_fts MATCH ? AND source = 'note'`,
		ftsQuotePhrase(name)).Scan(&n); err != nil {
		return 0, fmt.Errorf("mention count for %q: %w", name, err)
	}
	return n, nil
}
