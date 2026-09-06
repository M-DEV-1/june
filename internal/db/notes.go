package db

import (
	"context"
	"database/sql"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"log/slog"
	"ora/internal/memory"
	"ora/internal/obs"
	oratext "ora/internal/text"
	"strings"
	"time"
)

// Note is a stable, user-stated fact. Always-on in implicit context.
type Note struct {
	ID        int64
	Content   string
	Kind      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ArchivedNote is a fact that consolidation replaced, as it was when it was replaced.
type ArchivedNote struct {
	NoteID     int64
	Content    string
	Kind       string
	CreatedAt  time.Time
	ArchivedAt time.Time
}

// ArchivedNotes returns every fact consolidation has replaced, oldest archive first. Input: none. Output: the archived rows, or an error from the store.
func (s *Store) ArchivedNotes(ctx context.Context) ([]ArchivedNote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT note_id, content, kind, created_at, archived_at FROM notes_archive ORDER BY archived_at, id`)
	if err != nil {
		return nil, fmt.Errorf("archived notes: %w", err)
	}
	defer rows.Close()
	var out []ArchivedNote
	for rows.Next() {
		var a ArchivedNote
		var created, archived sql.NullTime
		if err := rows.Scan(&a.NoteID, &a.Content, &a.Kind, &created, &archived); err != nil {
			return nil, fmt.Errorf("archived notes: scan: %w", err)
		}
		a.CreatedAt, a.ArchivedAt = created.Time, archived.Time
		out = append(out, a)
	}
	return out, rows.Err()
}

// normalizeNoteContent trims, collapses internal whitespace to single spaces, and lowercases — used by LogNote's dedup check to catch paraphrased restatements.
// Does not strip punctuation, so "user likes go" and "user likes go." still stay distinct rows.
func normalizeNoteContent(s string) string {
	return oratext.OneLine(strings.ToLower(s))
}

// LogNote inserts a note. Idempotent on (content, kind) — returns existing id.
// Also dedupes paraphrased restatements: existing notes of the same kind are compared via normalizeNoteContent, so "User likes Go" and "user likes go" collapse to one row without needing a semantic/embedding index.
// Storage keeps the original casing/whitespace though — the first-ever version of a fact wins and is what every later paraphrase resolves back to (GetNotes/ExistingNotes/RetrieveRelevant all depend on this original casing surviving).
//
// The exact-match (content, kind) unique index still backs the INSERT OR IGNORE path below for byte-identical restatements and is what actually guards concurrent identical inserts — the normalized-comparison scan above is an application-level, non-atomic check and doesn't itself prevent a race between two differently-cased paraphrases.
func (s *Store) LogNote(ctx context.Context, content, kind string) (int64, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.LogNote")
	defer span.End()

	if kind == "" {
		kind = "fact"
	}

	normalized := normalizeNoteContent(content)
	if id, found, err := s.findNoteByNormalizedContent(ctx, normalized, kind); err != nil {
		span.RecordError(err)
		return 0, err
	} else if found {
		span.SetAttributes(attribute.Int64("db.note_id", id))
		return id, nil
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO notes (content, kind) VALUES (?, ?)`,
		content, kind); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("insert note: %w", err)
	}

	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM notes WHERE content = ? AND kind = ?`,
		content, kind).Scan(&id); err != nil {
		span.RecordError(err)
		return 0, fmt.Errorf("read note id: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.note_id", id))

	// Async, best-effort embedding — same non-blocking pattern as LogEpisode (own context; see its doc comment for why). notes.domain doesn't exist as a column, so metadata just omits the "domain" key rather than sending it empty.
	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()
	if emb != nil && vidx != nil && strings.TrimSpace(content) != "" {
		go func(id int64, text string) {
			embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vec, err := emb.Embed(embedCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async note embed failed", "note_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"source":     "note",
				"kind":       string(memory.KindFact),
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := vidx.Add(embedCtx, fmt.Sprintf("note:%d", id), text, vec, meta); err != nil {
				slog.Error("async note vector index add failed", "note_id", id, "error", err)
			}
		}(id, content)
	}
	return id, nil
}

// findNoteByNormalizedContent scans existing notes of kind for one whose content normalizes to the same value as normalized. Used by LogNote to catch paraphrased restatements that the exact-match (content, kind) unique index would not.
func (s *Store) findNoteByNormalizedContent(ctx context.Context, normalized, kind string) (int64, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM notes WHERE kind = ?`, kind)
	if err != nil {
		return 0, false, fmt.Errorf("scan existing notes for dedup: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var existingContent string
		if err := rows.Scan(&id, &existingContent); err != nil {
			return 0, false, fmt.Errorf("scan existing note row: %w", err)
		}
		if normalizeNoteContent(existingContent) == normalized {
			return id, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("iterate existing notes: %w", err)
	}
	return 0, false, nil
}

// NotesSince returns the notes touched at or after since, newest first. Input: the context and the bound. Output: every note whose created_at or updated_at is at or after the bound, so a note written days ago and closed today still comes back — that is the day it belongs on. This is what GET /today reads instead of GetNotes, which has no bound at all and grows with the whole store.
func (s *Store) NotesSince(ctx context.Context, since time.Time) ([]Note, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.NotesSince")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, kind, created_at, updated_at FROM notes WHERE created_at >= ? OR updated_at >= ? ORDER BY id DESC`,
		sqliteUTC(since), sqliteUTC(since))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query notes since: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Content, &n.Kind, &n.CreatedAt, &n.UpdatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan note since: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("iterate notes since: %w", err)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(out)))
	return out, nil
}

// GetNotes returns all notes ordered newest first.
func (s *Store) GetNotes(ctx context.Context) ([]Note, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.GetNotes")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, kind, created_at, updated_at FROM notes ORDER BY id DESC`)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query notes: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Content, &n.Kind, &n.CreatedAt, &n.UpdatedAt); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan note: %w", err)
		}
		out = append(out, n)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(out)))
	return out, nil
}

// RelevantNotes returns up to limit note contents relevant to focus, instead of the full notes table — same relevance-gated shape RetrieveRelevant/GetImplicitContext already use, applied to the plain fact strings DeriveState expects (no "[note]" prefix). An empty focus returns nil directly, same reasoning as RetrieveRelevant.
func (s *Store) RelevantNotes(ctx context.Context, focus string, limit int) ([]string, error) {
	focus = strings.TrimSpace(focus)
	if focus == "" {
		return nil, nil
	}
	rows := limit
	if rows <= 0 {
		rows = 10
	}
	hits, err := s.searchMemoryWindow(ctx, focus, "note", time.Time{}, time.Time{}, rows)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Content)
	}
	return out, nil
}

// DeleteNote removes a note by id, erroring when no such note exists. FTS5 mirror is dropped via trigger. Its vector (if any) is deleted async/best-effort — same non-blocking pattern as LogNote's embed goroutine — so a vector-index error never fails the SQL delete the model is waiting on.
func (s *Store) DeleteNote(ctx context.Context, id int64) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.DeleteNote")
	defer span.End()

	res, err := s.db.ExecContext(ctx, `DELETE FROM notes WHERE id = ?`, id)
	if err != nil {
		span.RecordError(err)
		return err
	}

	// A no-op DELETE is not success, for the same reason UpdateNote guards its own: the model can hand us an id it invented, and reporting "deleted" tells the user their note is gone when it is not. Returning before the vector work below also stops the "note:<id>" Delete from evicting the vector of some other real note.
	if n, rerr := res.RowsAffected(); rerr != nil {
		span.RecordError(rerr)
		return fmt.Errorf("delete note: %w", rerr)
	} else if n == 0 {
		err := fmt.Errorf("no note with id %d", id)
		span.RecordError(err)
		return err
	}

	s.mu.RLock()
	vidx := s.vectorIndex
	s.mu.RUnlock()
	if vidx != nil {
		go func(id int64) {
			delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := vidx.Delete(delCtx, fmt.Sprintf("note:%d", id)); err != nil {
				slog.Error("async note vector delete failed", "note_id", id, "error", err)
			}
		}(id)
	}
	return nil
}

// UpdateNote overwrites the content of an existing note. FTS5 mirror is kept in sync via the notes_au trigger, and updated_at is refreshed atomically. The stale vector is deleted and the corrected content re-embedded async/best-effort, same non-blocking pattern as LogNote — a vector-index error never fails the SQL update.
func (s *Store) UpdateNote(ctx context.Context, id int64, content string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.UpdateNote")
	defer span.End()

	res, err := s.db.ExecContext(ctx,
		`UPDATE notes SET content = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		content, id)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("update note: %w", err)
	}
	span.SetAttributes(attribute.Int64("db.note_id", id))

	// A no-op UPDATE is not success: the model can hand us an id it invented, and reporting "updated" throws the user's correction away. Returning before the vector work below also stops the "note:<id>" Delete from firing on an id that may belong to some other real note.
	if n, rerr := res.RowsAffected(); rerr != nil {
		span.RecordError(rerr)
		return fmt.Errorf("update note: %w", rerr)
	} else if n == 0 {
		err := fmt.Errorf("no note with id %d", id)
		span.RecordError(err)
		return err
	}

	s.mu.RLock()
	emb, vidx := s.embedder, s.vectorIndex
	s.mu.RUnlock()
	if vidx != nil {
		go func(id int64, text string) {
			vecCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			vecID := fmt.Sprintf("note:%d", id)
			if err := vidx.Delete(vecCtx, vecID); err != nil {
				slog.Error("async note vector delete (pre-update) failed", "note_id", id, "error", err)
			}
			if emb == nil || strings.TrimSpace(text) == "" {
				return
			}
			vec, err := emb.Embed(vecCtx, "RETRIEVAL_DOCUMENT", text)
			if err != nil {
				slog.Error("async note re-embed failed", "note_id", id, "error", err)
				return
			}
			meta := map[string]string{
				"source":     "note",
				"kind":       string(memory.KindFact),
				"created_at": time.Now().UTC().Format(time.RFC3339),
			}
			if err := vidx.Add(vecCtx, vecID, text, vec, meta); err != nil {
				slog.Error("async note vector re-add failed", "note_id", id, "error", err)
			}
		}(id, content)
	}
	return nil
}

// NoteRef is a note's id paired with its content, which is all the consolidation pass needs to reconcile against. Alias of the memory type for the reason given on db.Thread.
type NoteRef = memory.NoteRef

// ExistingNotes returns id+content for every stored note of kind "fact". Used by the memory compiler to feed the reconciliation LLM call and by note consolidation to feed the curation call.
// Both of those calls hand the notes to a model whose job is to merge and drop entries, and both write the result back through ReplaceAllNotes, so only the kind they are allowed to rewrite is shown to them. Other kinds — meeting minutes above all, which are the only record of what was said in a call — are never offered up for curation.
func (s *Store) ExistingNotes(ctx context.Context) ([]memory.NoteRef, error) {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ExistingNotes")
	defer span.End()

	rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM notes WHERE kind = 'fact' ORDER BY id ASC`)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("query existing notes: %w", err)
	}
	defer rows.Close()

	var out []memory.NoteRef
	for rows.Next() {
		var n memory.NoteRef
		if err := rows.Scan(&n.ID, &n.Content); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scan note ref: %w", err)
		}
		out = append(out, n)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(out)))
	return out, nil
}

// ReplaceAllNotes atomically swaps every note of kind "fact" for a curated set, used by periodic note consolidation. The new notes are written as kind "fact" too; FTS5 mirror stays in sync via the per-row notes_ad / notes_ai triggers.
// Notes of any other kind are left exactly as they are, rows and vectors both: meeting minutes live in this table under kind "meeting" and are the only record of what was said in a call, so consolidation must not be able to reach them.
// The caller must guarantee contents is non-empty — an empty swap would wipe the facts — but we defend against it here too.
func (s *Store) ReplaceAllNotes(ctx context.Context, contents []string) error {
	tracer := obs.GetTracer(ctx, "ora.db")
	ctx, span := tracer.Start(ctx, "DB.ReplaceAllNotes")
	defer span.End()

	if len(contents) == 0 {
		return fmt.Errorf("replace all notes: refusing to wipe table with empty set")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful Commit

	// Selected before the DELETE below so the old ids' vectors can be cleaned up — ReplaceAllNotes renumbers the facts it replaces (new AUTOINCREMENT ids on re-insert), so every replaced note's vector would otherwise become a permanent orphan. The kind filter matches the DELETE exactly: a vector is only deleted when its row is.
	var oldIDs []int64
	idRows, err := tx.QueryContext(ctx, `SELECT id FROM notes WHERE kind = 'fact'`)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: select old ids: %w", err)
	}
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			span.RecordError(err)
			return fmt.Errorf("replace notes: scan old id: %w", err)
		}
		oldIDs = append(oldIDs, id)
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: iterate old ids: %w", err)
	}

	// The facts being replaced are the source of the merged ones; keep them where a wrong merge can be traced and undone.
	if _, err := tx.ExecContext(ctx, `INSERT INTO notes_archive (note_id, content, kind, created_at, updated_at) SELECT id, content, kind, created_at, updated_at FROM notes WHERE kind = 'fact'`); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: archive: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM notes WHERE kind = 'fact'`); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: clear: %w", err)
	}

	for _, c := range contents {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO notes (content, kind) VALUES (?, 'fact')`, c); err != nil {
			span.RecordError(err)
			return fmt.Errorf("replace notes: insert: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		return fmt.Errorf("replace notes: commit: %w", err)
	}
	span.SetAttributes(attribute.Int("db.note_count", len(contents)))

	s.mu.RLock()
	vidx := s.vectorIndex
	s.mu.RUnlock()
	if vidx != nil && len(oldIDs) > 0 {
		go func(ids []int64) {
			delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, id := range ids {
				if err := vidx.Delete(delCtx, fmt.Sprintf("note:%d", id)); err != nil {
					slog.Error("async note vector delete (replace-all) failed", "note_id", id, "error", err)
				}
			}
		}(oldIDs)
	}
	return nil
}
