// This file is the procedures stage: once a night it reads back the screen-tool asks that worked and writes each distinct goal up as one plain "How I did X" note, so that asking for the same thing again recalls the way that worked instead of feeling across the screen from scratch. Nothing machine-facing survives the rendering — no accessibility refs, no object paths, no raw tool output — because these notes are read back as memory, in the words a person would use.
package dream

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"june/internal/db"
)

const (
	// procedureNoteKind is the notes.kind a procedure is stored under, keeping it apart from the memory compiler's facts, meeting minutes and action items.
	procedureNoteKind = "procedure"
	// procedurePrefixWord heads every procedure note. The goal and a colon follow it, and that whole head is the exact prefix the already-written check matches on.
	procedurePrefixWord = "How I did "
	// procedureRunCap is how many of the newest act runs the stage reads. ActRuns filters by count alone, so this count stands in for "the day's runs": at a few dozen screen asks a day it covers today and a stretch before it, and re-reading an older run costs nothing because a goal that already has its note is skipped.
	procedureRunCap = 200
	// procedureStepCap is the most plain steps one note may carry. A run longer than this was feeling its way around rather than following a procedure, so it is not written up at all.
	procedureStepCap = 12
)

// proceduresReport is what the procedures stage hands the morning report: how many usable ok runs it read, how many distinct goals they covered, how many notes it wrote, and how many goals it passed over because a note for them already existed.
type proceduresReport struct {
	runs    int
	goals   int
	written int
	skipped int
}

// procedureDraft is one goal's best candidate so far: the goal in the wording of the run that won, and that run's steps already rendered in plain words.
type procedureDraft struct {
	goal  string
	steps []string
}

// proceduresStage turns the recent successful screen-tool runs into "How I did X" notes, one per distinct goal, and returns what it did. Input: the night key, used only for logging. Output: the report, or a store error. Goals are grouped on their trimmed, case-folded text; the shortest successful run wins a goal; a goal whose note already exists is skipped, which is what makes running the stage twice in a row write nothing the second time.
func (r *Runner) proceduresStage(ctx context.Context, night string) (proceduresReport, error) {
	var rep proceduresReport

	runs, err := r.store.ActRuns(ctx, procedureRunCap)
	if err != nil {
		return rep, err
	}

	best := map[string]procedureDraft{}
	var keys []string
	for _, run := range runs {
		if run.Outcome != "ok" {
			continue
		}
		goal := strings.TrimSpace(run.Question)
		if goal == "" {
			continue
		}
		steps := db.RenderActSteps(run.Steps)
		if len(steps) == 0 || len(steps) > procedureStepCap {
			continue
		}
		rep.runs++

		key := strings.ToLower(goal)
		prev, seen := best[key]
		if seen && len(prev.steps) <= len(steps) {
			continue
		}
		if !seen {
			keys = append(keys, key)
		}
		best[key] = procedureDraft{goal: goal, steps: steps}
	}
	rep.goals = len(keys)
	if rep.goals == 0 {
		return rep, nil
	}

	known, err := r.store.NotesOfKindSince(ctx, procedureNoteKind, time.Time{})
	if err != nil {
		return rep, err
	}

	sort.Strings(keys)
	for _, key := range keys {
		draft := best[key]
		if procedureKnown(known, draft.goal) {
			rep.skipped++
			continue
		}
		content := procedureNote(draft.goal, draft.steps)
		if _, err := r.store.LogNote(ctx, content, procedureNoteKind); err != nil {
			return rep, err
		}
		known = append(known, db.Note{Content: content, Kind: procedureNoteKind})
		rep.written++
	}
	return rep, nil
}

// procedureKnown reports whether one of the existing procedure notes is already about this goal, matched on the exact "How I did <goal>:" head, trimmed and case-folded the same way the goals themselves are grouped. Input: the notes already stored and one goal. Output: true when the goal has been written up before.
func procedureKnown(known []db.Note, goal string) bool {
	head := strings.ToLower(procedureHead(goal))
	for _, n := range known {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(n.Content)), head) {
			return true
		}
	}
	return false
}

// procedureHead is the prefix every procedure note starts with: the fixed opening words, the goal in its own wording, and a colon.
func procedureHead(goal string) string {
	return procedurePrefixWord + goal + ":"
}

// procedureNote renders the note a goal's shortest successful run becomes: the head, then the plain steps in call order separated by commas, ending in a full stop.
func procedureNote(goal string, steps []string) string {
	return procedureHead(goal) + " " + strings.Join(steps, ", ") + "."
}

// procedureLine renders the morning report's one line about the procedures stage, added to the night's notes only when the stage actually wrote something.
func procedureLine(rep proceduresReport) string {
	if rep.written == 1 {
		return "I wrote up one new procedure from a screen-tool run that worked."
	}
	return fmt.Sprintf("I wrote up %d new procedures from the screen-tool runs that worked.", rep.written)
}
