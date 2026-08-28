package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ora/internal/obs"
)

// PersonalEntry is one thing known for certain about the user: who they are, a person in their life, a preference they stated. Keyed by subject, edited in place, and only ever written from something the user said themselves.
// Deliberately not a note. Notes are the derived world model — inferred, aged, compacted, sometimes wrong. Personal context is none of those things, and keeping it in its own table is what makes it impossible for the memory compiler or the note consolidator to rewrite it.
type PersonalEntry struct {
	Subject   string
	Content   string
	UpdatedAt time.Time
}

// SetPersonalContext writes content under subject, replacing whatever was there before. The subject is the key, so saying the same thing twice edits one row instead of adding a second — that is the whole point of the table.
// Input: a short plain or kebab-case subject ("identity", "trupti-hosmani") and the entry's prose. Output: an error if either is blank or the write fails.
func (s *Store) SetPersonalContext(ctx context.Context, subject, content string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.SetPersonalContext")
	defer span.End()

	subject, content = strings.TrimSpace(subject), strings.TrimSpace(content)
	if subject == "" {
		return fmt.Errorf("personal context needs a subject")
	}
	if content == "" {
		return fmt.Errorf("personal context needs content")
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO personal_context (subject, content, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(subject) DO UPDATE SET content = excluded.content, updated_at = CURRENT_TIMESTAMP`,
		subject, content); err != nil {
		span.RecordError(err)
		return fmt.Errorf("set personal context: %w", err)
	}
	return nil
}

// DeletePersonalContext removes the entry under subject. Deleting one that isn't there is not an error.
func (s *Store) DeletePersonalContext(ctx context.Context, subject string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.DeletePersonalContext")
	defer span.End()

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM personal_context WHERE subject = ?`, strings.TrimSpace(subject)); err != nil {
		span.RecordError(err)
		return fmt.Errorf("delete personal context: %w", err)
	}
	return nil
}

// PersonalContext returns every entry, ordered by subject. There is no search over this table and there never should be: it is small enough to inject whole into every prompt, which is why nothing here can go stale or be missed by a query.
func (s *Store) PersonalContext(ctx context.Context) ([]PersonalEntry, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.PersonalContext")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT subject, content, updated_at FROM personal_context ORDER BY subject ASC`)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query personal context: %w", err)
	}
	defer rows.Close()

	var entries []PersonalEntry
	for rows.Next() {
		var e PersonalEntry
		if err := rows.Scan(&e.Subject, &e.Content, &e.UpdatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan personal context: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate personal context: %w", err)
	}
	return entries, nil
}

// identityEntry is the clean prose the identity note becomes when it moves into personal context.
const identityEntry = "The user is Mahadevan KS — goes by Mahadevan; git handle M-DEV-1. He is the owner of this computer and the [me] speaker in every meeting recording."

// migrateIdentityNote moves the identity fact the memory compiler filed as a note into personal context, where it belongs, and deletes the note. It matches conservatively — kind 'fact', naming both "Mahadevan KS" and "owner of this computer" — so no other note can be caught by it.
// Idempotent in two ways: the note is gone after the first run, and the insert does nothing when an "identity" entry already exists, so a user edit is never overwritten if a similar note is ever written again.
func (s *Store) migrateIdentityNote() error {
	const match = `kind = 'fact' AND content LIKE '%Mahadevan KS%' AND content LIKE '%owner of this computer%'`

	if _, err := s.db.Exec(
		`INSERT INTO personal_context (subject, content, updated_at)
		 SELECT 'identity', ?, CURRENT_TIMESTAMP
		 WHERE EXISTS (SELECT 1 FROM notes WHERE `+match+`)
		 ON CONFLICT(subject) DO NOTHING`, identityEntry); err != nil {
		return fmt.Errorf("migrate identity note into personal context: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM notes WHERE ` + match); err != nil {
		return fmt.Errorf("delete migrated identity note: %w", err)
	}
	return nil
}
