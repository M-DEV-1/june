// This file is the storage for action items: the things somebody agreed to do, kept as ordinary notes rows under kind 'action' so they inherit the FTS mirror and vector index every other note has, and read back through memory.ParseAction. There is no table of their own on purpose — an action item is a note with a status, and the notes table already carries everything else it needs.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"june/internal/memory"
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

// DeleteMeetingActionItems removes the action items one meeting's minutes filed that are still open, for a meeting whose write-up the user has deleted. Input: the minutes as they were filed (without the duration marker) and when the meeting started. Output: how many items were removed.
// An item is this meeting's when its work, its meeting's name and the day it was raised all match what liftActionItems filed from these minutes: the name alone is shared by every instance of a recurring meeting, and the work alone by a later meeting that raised the same thing again, which AddActionItems never filed a second row for. Only open items go. One the user ticked, or one later writing closed, is something they acted on, and stays.
// They are deleted rather than dropped because the minutes were rejected, not the work: a dropped row still holds its key, so a second attempt at the same recording's minutes could never file the item again even when it really was agreed.
func (s *Store) DeleteMeetingActionItems(ctx context.Context, minutes string, startedAt time.Time) (int, error) {
	label := memory.MinutesLabel(minutes)
	filed := memory.ParseMinutesActions(minutes, label, startedAt)
	if len(filed) == 0 {
		return 0, nil
	}
	keys := make(map[string]bool, len(filed))
	for _, a := range filed {
		keys[actionKey(a)] = true
	}
	// The raised day is read back with time.Parse, so it is midnight UTC on the day the note names; it is compared as that string, and the meeting's own start as the local day liftActionItems wrote.
	day := startedAt.Local().Format(raisedDayFormat)

	all, err := s.actionItems(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, a := range all {
		if a.Status != memory.StatusOpen || a.Source != label || a.Raised.IsZero() || a.Raised.Format(raisedDayFormat) != day || !keys[actionKey(a)] {
			continue
		}
		if err := s.DeleteNote(ctx, a.NoteID); err != nil {
			return removed, fmt.Errorf("delete a meeting's action item: %w", err)
		}
		removed++
	}
	return removed, nil
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
			if e.when.Before(a.Created) || raisedThere(e, a) {
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

// closingEvidence is one piece of later writing a task might be finished in. Meeting is the name of the meeting it is the minutes of, "" for anything that is not a set of minutes, and day is the calendar day it was written on; together they are what keeps a meeting from closing the very items it raised.
type closingEvidence struct {
	label   string
	meeting string
	day     string
	text    string
	when    time.Time
}

// raisedThere reports whether a piece of evidence is the very minutes that raised this item. Input: one piece of evidence and one open item. Output: true only when both name the same meeting on the same calendar day.
// The name alone is not enough: every instance of a recurring meeting carries it, and the daily standup is precisely where "yes, I did that" gets said, so matching on the name would throw away the evidence of the one meeting most likely to hold it.
func raisedThere(e closingEvidence, a memory.ActionItem) bool {
	if e.meeting == "" || e.meeting != a.Source {
		return false
	}
	raised := a.Raised
	if raised.IsZero() {
		raised = a.Created
	}
	return e.day == raised.Local().Format(raisedDayFormat)
}

// evidenceKinds are the note kinds whose text can say a task is finished: a meeting's minutes and the memory compiler's own observations. Action notes are deliberately not among them — a task must not close itself.
var evidenceKinds = []string{"meeting", string(memory.KindFact)}

// closingEvidence reads everything written since that could say a task is done: meeting minutes, compiled notes, and the diary's day pages. Input: the earliest writing to read, zero for all of it. Output: each one's text with a label naming where it came from, for the closed item to point at.
// Only kind='day' diary rows count, which is what the "your day, " label already claims. The morning brief is generated from the open task list itself, so it names every open task and carries a completion word about one of them often enough to close the wrong one; the understanding doc, the dream reports, the week and month rollups and the task-notice watermark are not writing about a day at all.
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
			out = append(out, closingEvidence{label: label, meeting: meeting, day: day, text: n.Content, when: n.CreatedAt})
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT day, content, created_at FROM diary WHERE kind = 'day' AND created_at >= ? ORDER BY id`, sqliteUTC(since))
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

// SetActionText replaces the work an action item describes, leaving its status, priority, owner and provenance alone. Input: the note id the item is stored as, and the corrected work text. Output: an error when the id is not an action note.
// It exists because an action item is a rendered line, not a free-text row: writing the correction straight over the content through UpdateNote would take the "[status/priority]" prefix with it, ParseAction would stop reading the row, and the task would silently leave the brief and the Tasks screen.
func (s *Store) SetActionText(ctx context.Context, noteID int64, text string) error {
	return s.reviseAction(ctx, noteID, func(a *memory.ActionItem) error {
		a.Text = strings.TrimSpace(text)
		return nil
	})
}

// SetOwnerClass records whose task the user says this really is, overriding whatever OwnerClass would otherwise read from the owner the minutes named. Input: the note id and the class ("me", "them" or "unclear"). Output: an error when the class is not one of the three, or the id is not an action note — reporting a correction as applied when it was not throws the user's words away.
func (s *Store) SetOwnerClass(ctx context.Context, noteID int64, class string) error {
	if !memory.ValidOwnerClass(class) {
		return fmt.Errorf("%q is not an owner class", class)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE notes SET owner_class = ? WHERE id = ? AND kind = ?`, class, noteID, memory.ActionNoteKind)
	if err != nil {
		return fmt.Errorf("set owner class: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set owner class: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no action item with id %d", noteID)
	}
	return nil
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

// ErrNotActionItem says an id names no tracked action item — either there is no notes row of that id under kind 'action', or there is one whose line no longer parses. Callers that can treat the row as an ordinary note read it with errors.Is; the revise tool uses it to tell "correct this item's work text" from "overwrite this plain note".
var ErrNotActionItem = errors.New("not an action item")

// reviseAction reads one action note, applies revise to it, and writes the re-rendered line back through UpdateNote so the FTS mirror and the vector index follow. An id that is not an action note is an error rather than a silent no-op.
func (s *Store) reviseAction(ctx context.Context, noteID int64, revise func(*memory.ActionItem) error) error {
	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM notes WHERE id = ? AND kind = ?`, noteID, memory.ActionNoteKind).Scan(&content)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no action item with id %d: %w", noteID, ErrNotActionItem)
	}
	if err != nil {
		return fmt.Errorf("read action item: %w", err)
	}
	a, ok := memory.ParseAction(content)
	if !ok {
		return fmt.Errorf("note %d is not a readable action item: %w", noteID, ErrNotActionItem)
	}
	if err := revise(&a); err != nil {
		return err
	}
	return s.UpdateNote(ctx, noteID, a.Note())
}

// actionItems reads every action note and parses it, oldest first. A note that no longer parses is skipped rather than fatal: the user is free to edit a note by hand, and one mangled line must not take the rest of the brief down with it.
func (s *Store) actionItems(ctx context.Context) ([]memory.ActionItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, content, created_at, owner_class FROM notes WHERE kind = ? ORDER BY id`, memory.ActionNoteKind)
	if err != nil {
		return nil, fmt.Errorf("query action items: %w", err)
	}
	defer rows.Close()

	var out []memory.ActionItem
	for rows.Next() {
		var id int64
		var content, ownerOverride string
		var created time.Time
		if err := rows.Scan(&id, &content, &created, &ownerOverride); err != nil {
			return nil, fmt.Errorf("scan action item: %w", err)
		}
		if a, ok := memory.ParseAction(content); ok {
			a.NoteID, a.Created, a.OwnerOverride = id, created, ownerOverride
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
