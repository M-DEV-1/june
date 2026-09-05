// This file is the storage for action items: the things somebody agreed to do, kept as ordinary notes rows under kind 'action' so they inherit the FTS mirror and vector index every other note has, and read back through memory.ParseAction. There is no table of their own on purpose — an action item is a note with a status, and the notes table already carries everything else it needs.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"ora/internal/memory"
)

// ActionItem is one thing somebody agreed to do, stored as a notes row. Alias of the memory type for the reason given on db.Thread.
type ActionItem = memory.ActionItem

// AddActionItems stores each item that is not already on file. Input: the items parsed out of one meeting's minutes. Output: how many were new. Matching is on the owner and the work, never the rendered line, because a closed item's line differs from its open one only by its status tag — dedupe on the whole line would let re-filing the same minutes resurrect work the user had already marked done.
func (s *Store) AddActionItems(ctx context.Context, items []memory.ActionItem) (int, error) {
	known, err := s.actionKeys(ctx)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, a := range items {
		key := actionKey(a)
		if known[key] {
			continue
		}
		if _, err := s.LogNote(ctx, a.Note(), memory.ActionNoteKind); err != nil {
			return added, fmt.Errorf("file action item: %w", err)
		}
		known[key] = true
		added++
	}
	return added, nil
}

// OpenActionItems returns the action items still owed by the user, oldest first. Deliberately unwindowed: an owed task does not stop being owed because the meeting that raised it was a while ago, which is the whole reason action items live outside the minutes that mention them.
// Rows filed under somebody else's name are skipped even though they are on file — a row added before this filter existed, or added despite it by a hand edit, must not surface everywhere action items are read just because it exists.
func (s *Store) OpenActionItems(ctx context.Context) ([]memory.ActionItem, error) {
	all, err := s.actionItems(ctx)
	if err != nil {
		return nil, err
	}
	var open []memory.ActionItem
	for _, a := range all {
		if a.Status == memory.StatusOpen && a.Mine() {
			open = append(open, a)
		}
	}
	return open, nil
}

// SetActionStatus marks one action item open, done or dropped, leaving everything else about it alone. Input: the note id the item is stored as, and the new status. Output: an error when the id is not an action note or the status is not one of the three — reporting a correction as applied when it was not throws the user's words away.
func (s *Store) SetActionStatus(ctx context.Context, noteID int64, status string) error {
	return s.reviseAction(ctx, noteID, func(a *memory.ActionItem) error {
		if !memory.ValidStatus(status) {
			return fmt.Errorf("%q is not an action status", status)
		}
		a.Status = status
		return nil
	})
}

// SetActionPriority sets how much one action item matters, leaving its status alone. Priority is only ever the user's to set: minutes record who agreed to what, never how much it matters.
func (s *Store) SetActionPriority(ctx context.Context, noteID int64, priority string) error {
	return s.reviseAction(ctx, noteID, func(a *memory.ActionItem) error {
		if !memory.ValidPriority(priority) {
			return fmt.Errorf("%q is not an action priority", priority)
		}
		a.Priority = priority
		return nil
	})
}

// reviseAction reads one action note, applies revise to it, and writes the re-rendered line back through UpdateNote so the FTS mirror and the vector index follow. An id that is not an action note is an error rather than a silent no-op.
func (s *Store) reviseAction(ctx context.Context, noteID int64, revise func(*memory.ActionItem) error) error {
	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM notes WHERE id = ? AND kind = ?`, noteID, memory.ActionNoteKind).Scan(&content)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no action item with id %d", noteID)
	}
	if err != nil {
		return fmt.Errorf("read action item: %w", err)
	}
	a, ok := memory.ParseAction(content)
	if !ok {
		return fmt.Errorf("note %d is not a readable action item", noteID)
	}
	if err := revise(&a); err != nil {
		return err
	}
	return s.UpdateNote(ctx, noteID, a.Note())
}

// actionItems reads every action note and parses it, oldest first. A note that no longer parses is skipped rather than fatal: the user is free to edit a note by hand, and one mangled line must not take the rest of the brief down with it.
func (s *Store) actionItems(ctx context.Context) ([]memory.ActionItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM notes WHERE kind = ? ORDER BY id`, memory.ActionNoteKind)
	if err != nil {
		return nil, fmt.Errorf("query action items: %w", err)
	}
	defer rows.Close()

	var out []memory.ActionItem
	for rows.Next() {
		var id int64
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("scan action item: %w", err)
		}
		if a, ok := memory.ParseAction(content); ok {
			a.NoteID = id
			out = append(out, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate action items: %w", err)
	}
	return out, nil
}

// actionKeys is the set of work already on file, in any status, for AddActionItems to skip.
func (s *Store) actionKeys(ctx context.Context) (map[string]bool, error) {
	all, err := s.actionItems(ctx)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(all))
	for _, a := range all {
		keys[actionKey(a)] = true
	}
	return keys, nil
}

// actionKey identifies one piece of owed work by its owner and text alone, normalised the same way LogNote normalises a note, so casing and spacing drift between two filings of the same minutes does not read as a new item.
func actionKey(a memory.ActionItem) string {
	return normalizeNoteContent(strings.TrimSpace(a.Owner) + " — " + strings.TrimSpace(a.Text))
}
