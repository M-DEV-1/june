// This file is the storage for action items: the things somebody agreed to do, kept as ordinary notes rows under kind 'action' so they inherit the FTS mirror and vector index every other note has, and read back through memory.ParseAction. There is no table of their own on purpose — an action item is a note with a status, and the notes table already carries everything else it needs.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
// Only the user's own work: an item owed by a named other person, and an item nobody was named for, are both on file and both readable through ActionItemsByOwner, but neither is a task of his.
func (s *Store) OpenActionItems(ctx context.Context) ([]memory.ActionItem, error) {
	all, err := s.ActionItemsByOwner(ctx, memory.OwnerMe)
	if err != nil {
		return nil, err
	}
	var open []memory.ActionItem
	for _, a := range all {
		if a.Status == memory.StatusOpen {
			open = append(open, a)
		}
	}
	return open, nil
}

// ActionItemsByOwner returns the action items whose owner falls in one class, oldest first, in every status. Input: "me", "them", "unclear", or "all" for no filter. Output: the matching items, each with its note id and the date its row was written.
// Whose an item is comes from the owner the minutes named, read against the personal-context identity entry — it is worked out on every read rather than stored, so a corrected identity re-sorts the whole list and no row ever has to be rewritten.
func (s *Store) ActionItemsByOwner(ctx context.Context, owner string) ([]memory.ActionItem, error) {
	all, err := s.actionItems(ctx)
	if err != nil {
		return nil, err
	}
	identity := s.Identity(ctx)
	var out []memory.ActionItem
	for _, a := range all {
		if owner == "all" || a.OwnerClass(identity) == owner {
			out = append(out, a)
		}
	}
	return out, nil
}

// Identity is the personal-context entry that says who the user is, or "" when nothing on file says. It is what tells the user's own name in a meeting's action items from everybody else's.
func (s *Store) Identity(ctx context.Context) string {
	entries, err := s.PersonalContext(ctx)
	if err != nil {
		slog.Warn("could not read who the user is", "error", err)
		return ""
	}
	for _, e := range entries {
		if e.Subject == identitySubject {
			return e.Content
		}
	}
	return ""
}

// identitySubject is the personal-context key the user's own identity is filed under.
const identitySubject = "identity"

// CloseDoneActionItems closes the user's open tasks that some later piece of writing says are finished. Input: how far back to read evidence from (zero for all of it). Output: how many items were closed.
// Evidence is a meeting's minutes, a day's diary page, or a compiled note, and it only counts when it was written after the task was raised: the meeting that raises "deploy the PR" must not also close it. Nothing is ever deleted — a closed item keeps its line and gains the name of the evidence that closed it, so the user can see why it moved and put it back if the evidence was wrong.
func (s *Store) CloseDoneActionItems(ctx context.Context, since time.Time) (int, error) {
	open, err := s.OpenActionItems(ctx)
	if err != nil {
		return 0, err
	}
	if len(open) == 0 {
		return 0, nil
	}
	evidence, err := s.closingEvidence(ctx, since)
	if err != nil {
		return 0, err
	}

	closed := 0
	for _, a := range open {
		for _, e := range evidence {
			// The minutes that raised an item must never close it: they are filed in the same second as the item and always talk about the same work. Everything else is fair evidence as long as it was written no earlier than the item.
			if e.when.Before(a.Created) || (e.meeting != "" && e.meeting == a.Source) {
				continue
			}
			if !memory.EvidenceCloses(a, e.text) {
				continue
			}
			if err := s.SetActionDone(ctx, a.NoteID, e.label); err != nil {
				slog.Warn("could not close an action item the evidence says is finished", "note_id", a.NoteID, "error", err)
				break
			}
			slog.Info("closed an action item on the evidence of later writing", "note_id", a.NoteID, "evidence", e.label)
			closed++
			break
		}
	}
	return closed, nil
}

// SetActionDone marks one action item done and records what said so. Input: the note id and a label naming the evidence — the meeting, the day's page or the note. Output: an error when the id is not an action note.
func (s *Store) SetActionDone(ctx context.Context, noteID int64, doneSource string) error {
	return s.reviseAction(ctx, noteID, func(a *memory.ActionItem) error {
		a.Status, a.DoneSource = memory.StatusDone, doneSource
		return nil
	})
}

// closingEvidence is one piece of later writing a task might be finished in. Meeting is the name of the meeting it is the minutes of, "" for anything that is not a set of minutes, and is what keeps a meeting from closing the very items it raised.
type closingEvidence struct {
	label   string
	meeting string
	text    string
	when    time.Time
}

// evidenceKinds are the note kinds whose text can say a task is finished: a meeting's minutes and the memory compiler's own observations. Action notes are deliberately not among them — a task must not close itself.
var evidenceKinds = []string{"meeting", string(memory.KindFact)}

// closingEvidence reads everything written since that could say a task is done: meeting minutes, compiled notes, and the diary's day pages and morning briefs. Input: the earliest writing to read, zero for all of it. Output: each one's text with a label naming where it came from, for the closed item to point at.
func (s *Store) closingEvidence(ctx context.Context, since time.Time) ([]closingEvidence, error) {
	var out []closingEvidence
	for _, kind := range evidenceKinds {
		notes, err := s.NotesOfKindSince(ctx, kind, since)
		if err != nil {
			return nil, err
		}
		for _, n := range notes {
			day := n.CreatedAt.Local().Format(raisedDayFormat)
			label, meeting := kind+" "+day, ""
			if kind == "meeting" {
				if meeting = memory.MinutesLabel(n.Content); meeting != "" {
					label = meeting + " " + day
				}
			}
			out = append(out, closingEvidence{label: label, meeting: meeting, text: n.Content, when: n.CreatedAt})
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT day, content, created_at FROM diary WHERE created_at >= ? ORDER BY id`, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("query the diary for evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var day, content string
		var when time.Time
		if err := rows.Scan(&day, &content, &when); err != nil {
			return nil, fmt.Errorf("scan a diary entry: %w", err)
		}
		out = append(out, closingEvidence{label: "your day, " + day, text: content, when: when})
	}
	return out, rows.Err()
}

// raisedDayFormat is the calendar day an evidence label names, the same key the diary and the dreaming loop use.
const raisedDayFormat = "2006-01-02"

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
	rows, err := s.db.QueryContext(ctx, `SELECT id, content, created_at FROM notes WHERE kind = ? ORDER BY id`, memory.ActionNoteKind)
	if err != nil {
		return nil, fmt.Errorf("query action items: %w", err)
	}
	defer rows.Close()

	var out []memory.ActionItem
	for rows.Next() {
		var id int64
		var content string
		var created time.Time
		if err := rows.Scan(&id, &content, &created); err != nil {
			return nil, fmt.Errorf("scan action item: %w", err)
		}
		if a, ok := memory.ParseAction(content); ok {
			a.NoteID, a.Created = id, created
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
