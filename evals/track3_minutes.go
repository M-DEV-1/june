package main

// Track 3 scores the meeting minutes the recorder writes. The rules come from internal/recorder/minutes.go's minutesInstruction (lines 15-52): two attendee lists split by evidence of presence, the recording person folded into the named list once known and never appearing twice, no name taken from a repository page or a document, and action items with owners. The material is a recordings directory's minutes.md, with transcript.md handed to the judge alongside so it can check a claim against what was actually said.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ora/internal/text"
)

// track3Result is one minutes file scored against the attendee and taste rules.
type track3Result struct {
	Dir      string             `json:"-"`
	Criteria map[string]verdict `json:"criteria"`
	Err      string             `json:"-"`
}

// minutesIDs is the criterion order the scorecard prints.
var minutesIDs = []string{"M1", "M2", "M3", "M4", "M5", "M6"}

// minutesNames labels each criterion in the scorecard so a reader does not have to hold the rubric in their head.
var minutesNames = map[string]string{
	"M1": "no machine identifiers in the prose",
	"M2": "recorder is one named entry, never also a separate bullet",
	"M3": "in-meeting and mentioned-only are separated by evidence",
	"M4": "no narration of how people were identified",
	"M5": "action items each have an owner",
	"M6": "nothing asserted that the transcript and screen context cannot support",
}

const track3Instruction = `You are auditing the minutes an assistant wrote from an automatically recorded meeting.

You are given the minutes, and the transcript they were written from. The transcript has exactly two speaker labels: [me] is the person whose computer recorded the meeting, and [call] is every remote voice pooled into one label — the recording cannot tell those voices apart. Names come only from what the meeting app showed on screen (a participant tile, a "presenting" label, a chat sender) or from the talk itself (someone introduces themselves, or is addressed by name). A name on a repository page, in a commit, in a spreadsheet or in an open document names nobody.

Score each criterion "pass", "fail", or "na". Use "na" only when the minutes give no occasion to test it.

M1 The prose names no machine identifiers: no file path, branch name, repository name, database name, username, terminal prompt, or process name. Human-readable project and product names are fine. A technical term someone actually said in the meeting is fine.
M2 The person recording appears exactly once in the attendee list, under their real name where it is known, and never also as a separate "the person recording" bullet.
M3 The attendee list separates people with evidence they were in the call from names that only came up in talk or on screen. Anyone the minutes can only hedge about ("referenced via screen context", "mentioned somewhere") belongs in the second list, not the first.
M4 The minutes never narrate how a person was identified. The reader wants who was there, not the reasoning: no "inferred from", "based on screen context", "matching the recording path", "shown in the terminal owner".
M5 Every action item names an owner, or says the owner is unclear. An owner written as two people joined by a slash is not an owner.
M6 Nothing in the minutes asserts a fact the transcript cannot support. A name attached to a line the pooled [call] label could not have distinguished is a fail.

Reply with JSON only:
{"criteria":{"M1":{"verdict":"pass","why":"..."}, ... all six ...}}
Every "why" is one clause, under 20 words.`

// findMinutes returns every recordings subdirectory that has a minutes.md, most recent last. Input: the recordings root. Output: absolute directory paths.
func findMinutes(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "minutes.md")); err == nil {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// transcriptBudget caps how much transcript goes to the judge. Whole transcripts run to tens of thousands of runes and the attendee rules are decided in the first stretch, where people greet each other and the app shows who joined.
const transcriptBudget = 24000

// runTrack3 scores every minutes file with one judge call each. Input: the recording directories to score. Output: one result per directory.
func runTrack3(ctx context.Context, j *judge, dirs []string) []track3Result {
	results := make([]track3Result, 0, len(dirs))
	for _, dir := range dirs {
		r := track3Result{Dir: dir}
		minutes, err := os.ReadFile(filepath.Join(dir, "minutes.md"))
		if err != nil {
			r.Err = err.Error()
			results = append(results, r)
			continue
		}
		transcript, _ := os.ReadFile(filepath.Join(dir, "transcript.md"))
		material := fmt.Sprintf("MINUTES:\n%s\n\n---\n\nTRANSCRIPT (may be truncated):\n%s",
			minutes, truncateRunes(string(transcript), transcriptBudget))

		if err := j.ask(ctx, track3Instruction, material, &r); err != nil {
			r.Err = err.Error()
			fmt.Printf("  [%s] judge failed: %v\n", filepath.Base(dir), err)
			results = append(results, r)
			continue
		}
		passes, applicable := rate(collect(r.Criteria, minutesIDs))
		fmt.Printf("  [%s] %s\n", filepath.Base(dir), pct(passes, applicable))
		results = append(results, r)
	}
	return results
}

// truncateRunes caps s to n runes, cutting on a rune boundary so a multi-byte transcript is never sliced mid-character.
// truncateRunes cuts s to at most n runes, appending a "(truncated)" marker on its own line when it does.
func truncateRunes(s string, n int) string {
	cut := text.Runes(s, n)
	if cut == s {
		return s
	}
	return cut + "\n…(truncated)"
}

// failedMinutes lists the criteria this minutes file failed.
func (r track3Result) failedMinutes() []string {
	var out []string
	for _, id := range minutesIDs {
		if v, ok := r.Criteria[id]; ok && !v.passed() && !v.na() {
			out = append(out, id)
		}
	}
	return out
}

// joinIDs renders a criterion list for a table cell, saying "none" rather than leaving the cell blank.
func joinIDs(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}
