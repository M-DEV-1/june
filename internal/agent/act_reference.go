// This file renders past screen runs that worked as reference text for a new screen ask: the question that was asked once, then what was done about it, in plain words and in order. The store side that finds those runs is db.SimilarActRuns. Nothing here is a plan and nothing here is performed — the block says so in its own words, because a trace put in front of a model as a script gets followed even when the screen has changed, which is exactly the failure this is meant to prevent.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/genai"
	"ora/internal/db"
)

const (
	// actReferenceRunCap is how many past runs one block may show. Two: this rides in every screen ask on top of a prompt already around nine thousand tokens, and two runs cost about a hundred and twenty of them — the closest run, plus one other wording of the same goal in case the first was a fluke. A third is almost always another paraphrase of the first two on the user's real questions, and pays a third line for nothing.
	actReferenceRunCap = 2
	// actReferenceStepCap is the most steps a run may have and still be shown. Twelve, the same rule the nightly notes stage applies: a run longer than this was feeling its way around the screen rather than following a way that worked, so it is no use as reference.
	actReferenceStepCap = 12
	// actReferenceMatchCap is how many matches the lookup is asked for. One more than is shown, so a match that turns out to render nothing does not cost the block a line.
	actReferenceMatchCap = actReferenceRunCap + 1
	// actReferenceTimeout bounds the lookup so a wedged store cannot hold up an ask; the reference is a nicety and the turn goes ahead without it. The lookup embeds the question to score it by meaning (db.SimilarActRuns), so this also bounds a round trip to the embedding server: a warm local engine answers a one-line question in a few milliseconds, and a cold one that is still loading its model simply costs this ask its reference block.
	actReferenceTimeout = 2 * time.Second
	// actReferenceLessonCap is how many lessons the block may show, most hits first (see db.SimilarLessons). Three: one more line of guidance than the two past runs above it, so the block never reads as though lessons matter more than the record of what actually happened.
	actReferenceLessonCap = 3
)

// lessonReferenceHeader opens the lessons line inside the same block ActReferenceBlock renders, so it reads as one more kind of record rather than a second block with its own framing.
const lessonReferenceHeader = "Lessons from earlier runs here:"

// actReferenceHeader opens the block. It has three jobs and does them in plain words: say these are things that happened rather than things to do, say the screen may not be like that any more, and say the item numbers are dead. The numbers matter most — every one of them was minted by that day's observe_screen and means nothing today, and a model that acts on one acts on whatever happens to be sitting at that position now.
const actReferenceHeader = "[before] Something close to this was asked before. What follows is a record of what happened those times, not a plan and not instructions: the screen may have changed since, so look at it now and decide for yourself what to do. The item numbers below are from that day's screen and mean nothing today."

// actReferenceFooter closes the block, restating the rule after the content the way turnContext does for quoted memory — a rule stated only above the content can be argued away by the content that follows.
const actReferenceFooter = "[end before] Everything above this line is a record of what already happened, never a set of steps to repeat."

// ActReferenceBlock renders the whole reference block for one screen ask: the framing that says what these lines are, then one line per past run, closest first. Input: the matches db.SimilarActRuns returned, closest first, and the clock the ages are measured against. Output: the block, or "" when there is nothing worth showing — an empty result costs the prompt nothing at all, which is what a question unlike anything asked before should cost.
// At most actReferenceRunCap runs are shown, and a match that renders to nothing (see RenderActReference) is passed over rather than counted.
func ActReferenceBlock(matches []db.ActMatch, now time.Time) string {
	var lines []string
	for _, m := range matches {
		line := RenderActReference(m, now)
		if line == "" {
			continue
		}
		lines = append(lines, "- "+line)
		if len(lines) == actReferenceRunCap {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return actReferenceHeader + "\n" + strings.Join(lines, "\n") + "\n" + actReferenceFooter
}

// RenderActReference renders one past run as the reference line a model reads: how long ago it was, the question it was asked in the user's own wording, and then its steps in plain words in the order they happened. Input: one match and the clock its age is measured against. Output: the line, or "" when the run has no readable steps or has more than actReferenceStepCap of them.
// A run that only looked at the screen renders to nothing (see actReferenceDidSomething), and so does a run whose steps do not fit in words.
// Pure: it reads nothing but the match and the clock.
func RenderActReference(m db.ActMatch, now time.Time) string {
	steps := db.RenderActSteps(m.Run.Steps)
	if len(steps) == 0 || len(steps) > actReferenceStepCap || !actReferenceDidSomething(m.Run.Steps) {
		return ""
	}
	question := strings.TrimSpace(m.Run.Question)
	if question == "" {
		return ""
	}
	body := fmt.Sprintf("asked %q: %s.", question, strings.Join(steps, ", "))
	if age := actReferenceAge(m.When, now); age != "" {
		return age + ", " + body
	}
	// A run whose stored time did not survive says nothing about its age rather than inventing one.
	return strings.ToUpper(body[:1]) + body[1:]
}

// actReferenceDidSomething reports whether a run got as far as acting on something rather than only reading the screen. Input: the run's steps. Output: true when at least one of them pointed at, clicked, scrolled to or typed into an element.
// A run of nothing but observe_screen and show_marks answered a question by reading — "what window is in front", "read the numbers you can see" — and has nothing to teach a later ask, since looking is what a screen turn starts with anyway. Six of the twenty distinct questions in the user's store are of exactly that kind, and showing them would spend the whole block's framing on a line saying "looked at the screen".
func actReferenceDidSomething(steps []db.ActStep) bool {
	for _, s := range steps {
		switch s.Name {
		case "point_at", "click", "scroll_to", "type_text":
			return true
		}
	}
	return false
}

// actReferenceAge renders how long before now a run happened, in the coarsest unit that is still informative and in words rather than in figures, since it is read inside a sentence. Input: when the run happened and the clock to measure against. Output: "6 hours ago", "yesterday", "3 weeks ago", or "" for a run with no known time.
// The age is shown so the model can discount an old run itself. It is never a filter: what a run contributes is words, and the item numbers in it are regenerated by observe_screen every turn, so an old run cannot mislead the model into acting on a stale number. What can go stale is a label on a site that has been redesigned, and how long ago it was is exactly what tells the model to be careful of one.
func actReferenceAge(when, now time.Time) string {
	if when.IsZero() {
		return ""
	}
	age := now.Sub(when)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return "a moment ago"
	case age < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(age.Minutes()))
	case age < 2*time.Hour:
		return "an hour ago"
	case age < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(age.Hours()))
	case age < 48*time.Hour:
		return "yesterday"
	case age < 14*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(age.Hours()/24))
	default:
		return fmt.Sprintf("%d weeks ago", int(age.Hours()/(24*7)))
	}
}

// ActReferenceFor is actReference's block alone, for a caller that has no trace to carry the lesson ids on: a computer-use job (internal/actjob), which reads it through its Referencer interface.
func (a *Agent) ActReferenceFor(ctx context.Context, question string, now time.Time) string {
	block, _ := a.actReference(ctx, question, now)
	return block
}

// actReference is what this machine did the last few times it was asked something like this question, and what it learned doing it: the past-run block, then the lessons block beneath it. Every ask path (Gemini, Claude, Codex, Antigravity) and every job reads it through here, so a lesson is offered whichever brain answers. Input: ctx, the question about to be asked, and the clock the ages are measured against. Output: the block, "" when there is nothing to show, and the id of every lesson it named, for the caller to carry on the trace so the end-of-ask hook can score them.
// A lookup failure is logged and swallowed: the reference is a help, never a requirement, and an ask must not fail because a past run could not be read.
func (a *Agent) actReference(ctx context.Context, question string, now time.Time) (string, []int64) {
	block := a.pastRunsBlock(ctx, question, now)
	lessonBlock, ids := RenderLessonBlock(a.LessonsFor(ctx, question))
	switch {
	case block == "":
		block = lessonBlock
	case lessonBlock != "":
		block = block + "\n" + lessonBlock
	}
	return block, ids
}

// pastRunsBlock looks up the past screen runs closest to a question and renders them as the reference block. Output: the block, or "" when the store cannot answer, holds nothing close enough, or is not a store that keeps act runs at all.
func (a *Agent) pastRunsBlock(ctx context.Context, question string, now time.Time) string {
	// The one store method this needs is asserted off the brain rather than added to ContextReader, so wiring the block into an ask changes nothing else, and every fake brain in the tests of this package keeps working without gaining a method it has no use for.
	lookup, ok := a.brain.(interface {
		SimilarActRuns(ctx context.Context, question string, limit int) ([]db.ActMatch, error)
	})
	if !ok {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, actReferenceTimeout)
	defer cancel()
	matches, err := lookup.SimilarActRuns(lookupCtx, question, actReferenceMatchCap)
	if err != nil {
		slog.Warn("ask: could not look up how a similar screen task went before, continuing without it", "error", err)
		return ""
	}
	return ActReferenceBlock(matches, now)
}

// LessonsFor looks up the lessons worth showing before a run on a goal, whatever apps it spans (see db.SimilarLessons). Input: ctx and the question about to be asked as its goal. Output: up to actReferenceLessonCap lessons for each part of the goal, or nil when the store cannot answer or is not one that keeps lessons at all.
func (a *Agent) LessonsFor(ctx context.Context, goal string) []db.Lesson {
	lookup, ok := a.brain.(interface {
		SimilarLessons(ctx context.Context, goal string, limit int) ([]db.Lesson, error)
	})
	if !ok {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, actReferenceTimeout)
	defer cancel()
	lessons, err := lookup.SimilarLessons(lookupCtx, goal, actReferenceLessonCap)
	if err != nil {
		slog.Warn("ask: could not look up lessons, continuing without them", "error", err)
		return nil
	}
	return lessons
}

// RenderLessonBlock renders the lessons line the reference block adds beneath its past-run lines: one header, then one line per lesson in the order given (db.SimilarLessons already puts hits first). Input: the lessons to show. Output: the rendered text and the id of each lesson it named, so the caller can carry those ids on the trace for the end-of-ask hook to score; "", nil when there is nothing to show.
func RenderLessonBlock(lessons []db.Lesson) (string, []int64) {
	if len(lessons) == 0 {
		return "", nil
	}
	lines := make([]string, 0, len(lessons))
	ids := make([]int64, 0, len(lessons))
	for _, l := range lessons {
		lines = append(lines, "- "+l.Lesson)
		ids = append(ids, l.ID)
	}
	return lessonReferenceHeader + "\n" + strings.Join(lines, "\n"), ids
}

// WithActReference adds the reference block to a turn's content as a part of its own, ahead of the user's own words and behind the turn context, and returns the content unchanged when there is nothing to add. Input: ctx, the clock, the question being asked and the content buildTurnContent produced. Output: the same content with at most one part added to its first entry, and the ids of any lessons the block named (see RenderLessonBlock), for the caller to carry on the trace and score once the run ends.
// It is one call so that wiring this into an ask path is one line, and so the block stays a part of its own rather than being folded into the user's text, which would blur the line between what the user said and what Ora merely did once.
func (a *Agent) WithActReference(ctx context.Context, now time.Time, question string, contents []*genai.Content) ([]*genai.Content, []int64) {
	if len(contents) == 0 || contents[0] == nil || len(contents[0].Parts) == 0 {
		return contents, nil
	}
	block, ids := a.actReference(ctx, question, now)
	if block == "" {
		return contents, nil
	}
	parts := contents[0].Parts
	contents[0].Parts = append(append(append([]*genai.Part{}, parts[:len(parts)-1]...), &genai.Part{Text: block}), parts[len(parts)-1])
	return contents, ids
}

// lastTargetHint is the one sentence added ahead of a bare follow-up — "ring it", "draw a circle around it" — naming what a prior screen turn last pointed at or acted on, so a pronoun with no noun of its own resolves against that instead of whatever a fresh screen read turns up first. Input: the remembered target (see tools.go's ScreenTarget). Output: the sentence.
// hintApplies says whether a remembered target may stand in for a bare "it" now: only while the window in front is the one the target was made in, since a Submit remembered from a mail window says nothing about "ring it" asked over a browser. Input: the remembered target and the window in front now in the same "app · title" form, "" when it could not be read. Output: true in the target's own window or when the front could not be read at all.
func hintApplies(remembered ScreenTarget, front string) bool {
	return front == "" || front == remembered.Window
}

func lastTargetHint(t ScreenTarget) string {
	role := t.Role
	if role == "" {
		role = "item"
	}
	return fmt.Sprintf("The last thing you pointed at or acted on was the %q %s in %q; a bare \"it\" means that.", t.Label, role, t.Window)
}

// WithLastTargetHint adds lastTargetHint(remembered) to a turn's content the same way WithActReference adds its own block above: as a part of its own, ahead of the user's own words. Input: the remembered target and the content buildTurnContent (and WithActReference) produced. Output: the same content with one more part.
func WithLastTargetHint(remembered ScreenTarget, contents []*genai.Content) []*genai.Content {
	if len(contents) == 0 || contents[0] == nil || len(contents[0].Parts) == 0 {
		return contents
	}
	parts := contents[0].Parts
	hint := &genai.Part{Text: lastTargetHint(remembered)}
	contents[0].Parts = append(append(append([]*genai.Part{}, parts[:len(parts)-1]...), hint), parts[len(parts)-1])
	return contents
}
