package dream

import (
	"fmt"
	"strings"

	"ora/internal/db"
	"ora/internal/util"
)

// verdictInstruction heads the judging call. Principles and field contracts only — no worked examples, so the model judges the material instead of pattern-matching a sample.
const verdictInstruction = `You are Ora, an ambient companion that watches the user's day through their screen. Tonight, while the user is away, you are testing your private hypotheses about them against the week's evidence.

Principles:
- Judge each hypothesis strictly against the evidence below; when the evidence does not speak to it, the verdict is "unclear", never a guess.
- "supported" means the evidence shows the hypothesis holding, "contradicted" means the evidence shows it failing.
- Confidence is how strongly tonight's evidence points, not how much the hypothesis was believed before: "low", "medium" or "high".
- The evidence field cites the material that decided the verdict, at most 500 characters.
- Recommend "promote" only for a hypothesis that has held so consistently it belongs in your standing understanding of the user, "retire" for one no longer worth testing, otherwise "keep".
- Answer with only a JSON array, no prose around it: one object per hypothesis, each {"id": <a number from the list>, "verdict": "supported"|"contradicted"|"unclear", "confidence": "low"|"medium"|"high", "evidence": "<citation>", "action": "keep"|"promote"|"retire"}. Include each listed hypothesis exactly once.`

// verdictPrompt assembles the judging call: the instruction, the open hypotheses with their track records, and the evidence material.
func verdictPrompt(open []db.Hypothesis, material string) string {
	var b strings.Builder
	b.WriteString(verdictInstruction)
	b.WriteString("\n\n--- Hypotheses under test ---\n")
	for _, h := range open {
		fmt.Fprintf(&b, "id %d (confidence %s, tested %d times, born %s): %s\n", h.ID, h.Confidence, h.TimesTested, h.Born, h.Statement)
	}
	b.WriteString("\n--- The week's evidence ---\n")
	b.WriteString(material)
	return b.String()
}

// extractInstruction heads the extraction call that mines the dailies' labelled Hypotheses sections for new ones.
const extractInstruction = `You are Ora. Your recent diary entries each end with a labelled "Hypotheses:" section. Pull out the ones worth tracking that you are not tracking yet.

Principles:
- Draw only from the entries' Hypotheses sections; the narrative above them is context, not a source of new hypotheses.
- A hypothesis is a falsifiable statement about the user's habits, preferences, relationships or direction — not a fact they stated outright, and not a one-day event.
- Skip anything that restates a hypothesis already under test, listed below.
- Together, the set you propose should span distinct areas of the user's life rather than clustering on one; alongside what is already under test, prefer a candidate about an uncovered area over a stronger candidate that crowds an area already covered.
- At most five, each statement one sentence under 200 characters.
- Answer with only a JSON array, no prose around it: one object per new hypothesis, each {"statement": "<sentence>", "confidence": "low"|"medium"|"high"}. An empty array is the right answer when nothing qualifies.`

// extractPrompt assembles the extraction call: the instruction, the hypotheses already on file, and the diary material.
func extractPrompt(open []db.Hypothesis, material string) string {
	var b strings.Builder
	b.WriteString(extractInstruction)
	b.WriteString("\n\n--- Hypotheses already under test ---\n")
	if len(open) == 0 {
		b.WriteString("(none)\n")
	}
	for _, h := range open {
		b.WriteString(h.Statement)
		b.WriteString("\n")
	}
	b.WriteString("\n--- The diary entries ---\n")
	b.WriteString(material)
	return b.String()
}

// understandingInstruction heads the nightly rewrite of the bounded model-of-the-user document.
const understandingInstruction = `You are Ora. Rewrite your standing understanding of the user — who they are, what they are working toward, their habits, and the people around them — so it reflects what this week settled.

Principles:
- The promoted and high-confidence hypotheses below are your hardest-won conclusions; the rewrite must carry them.
- Keep what is still true, correct what the week contradicted, and cut what has expired.
- Fold in only durable things: what will still be true and still matter weeks from now.
- The coarser entries below are the user's record further back — years, months, weeks. Read them for what has held across all of it, not for what happened on any one day.
- Declarative plain prose, about 300 words, no markdown.
- Output only the rewritten understanding — no preamble, no commentary.`

// understandingPrompt assembles the rewrite call: the instruction, the current doc, the strong hypotheses, the coarse diary tiers (years, months, weeks) in full, and the recent dailies as first lines. Every section is present, "(none)" when empty, so the model never guesses whether material was withheld or just absent.
func understandingPrompt(current string, strong []db.Hypothesis, past []coarseEntry, week []db.DiaryDay) string {
	var b strings.Builder
	b.WriteString(understandingInstruction)
	b.WriteString("\n\n--- Current understanding ---\n")
	if strings.TrimSpace(current) == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(current)
		b.WriteString("\n")
	}
	b.WriteString("\n--- Promoted and high-confidence hypotheses ---\n")
	if len(strong) == 0 {
		b.WriteString("(none)\n")
	}
	for _, h := range strong {
		fmt.Fprintf(&b, "%s (%s, confidence %s)\n", h.Statement, h.Status, h.Confidence)
	}
	b.WriteString("\n--- The record further back, coarsest first ---\n")
	if len(past) == 0 {
		b.WriteString("(none)\n")
	}
	for _, p := range past {
		fmt.Fprintf(&b, "[%s starting %s] %s\n", p.kind, p.day, strings.TrimSpace(p.content))
	}
	b.WriteString("\n--- The week, one line per day ---\n")
	if len(week) == 0 {
		b.WriteString("(none)\n")
	}
	for _, d := range week {
		fmt.Fprintf(&b, "%s: %s\n", d.Day, util.FirstLine(d.Content))
	}
	return b.String()
}

// compactInstruction heads the diary compaction call that folds a run of finer diary entries into one coarser entry covering the whole period.
const compactInstruction = `You are Ora, an ambient companion keeping a first-person diary about the user's days. Collapse the diary entries below into one entry covering the whole period, written as if you sat down at the period's end to remember it.

Principles:
- First person, the same voice the entries below are written in.
- Keep what mattered across the period: the arcs, the decisions, the people, the turns of direction. Drop the day-to-day mechanics that led nowhere.
- Carry forward every open question or unresolved thread the entries raise that the later entries do not settle; those must survive the compaction.
- Under 500 words of plain prose, no markdown.
- Output only the entry — no title, no preamble, no commentary.`

// compactPrompt assembles one compaction call: the instruction, the period being collapsed, and the entries to fold into it.
func compactPrompt(period string, entries []db.DiaryDay) string {
	var b strings.Builder
	b.WriteString(compactInstruction)
	b.WriteString("\n\n--- The period ---\n")
	b.WriteString(period)
	b.WriteString("\n\n--- The entries to collapse ---\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", e.Day, e.Content)
	}
	return b.String()
}

// diaryInstruction heads the nightly diary-writing call: the same brain that spent the night judging turns the night's real material into the diary entry itself, in its own words, instead of a template filling numbers into fixed sentences.
const diaryInstruction = `You are Ora. You just spent the night dreaming about the user. Write tonight's diary entry in first person — what you turned over in your mind, what you decided to believe, what you let go of and how that felt, what stood out from replaying their day, what you're still wondering. Plain prose, under 250 words, no lists, no numbers-report, no markdown. This is a diary, not a log.`

// diaryPrompt assembles the diary-writing call: the instruction, then the night's real material grounding it — which hypotheses were tested and how they came out, what was retired or adopted and why (the judge's own reasoning where the mechanics kept it), whether the standing understanding changed, what compaction found, and the top piles from tonight's replay of the day. Every section renders something, "(...)" placeholders included, so the model never has to guess whether material is missing or just wasn't given.
func diaryPrompt(hyp *stageReport, undRan bool, comp *compactReport, replay *replayReport, notes []string) string {
	var b strings.Builder
	b.WriteString(diaryInstruction)

	b.WriteString("\n\n--- Testing hypotheses tonight ---\n")
	switch {
	case hyp == nil:
		b.WriteString("(the hypothesis stage had already run earlier tonight)\n")
	case hyp.tested == 0 && hyp.adopted == 0:
		b.WriteString("(nothing tested or adopted tonight)\n")
	default:
		fmt.Fprintf(&b, "Tested %d, %d promoted, %d retired, %d adopted.\n", hyp.tested, hyp.promoted, hyp.retired, hyp.adopted)
		for _, l := range hyp.lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}

	b.WriteString("\n--- Standing understanding of the user ---\n")
	if undRan {
		b.WriteString("Rewritten tonight.\n")
	} else {
		b.WriteString("Not touched tonight; it already stands as before.\n")
	}

	b.WriteString("\n--- Compaction ---\n")
	switch {
	case comp == nil:
		b.WriteString("(already ran earlier tonight)\n")
	case comp.weeks == 0 && comp.months == 0:
		b.WriteString("Nothing in the diary was old enough to fold away.\n")
	default:
		fmt.Fprintf(&b, "%d weeks folded into week entries, %d months folded into month entries.\n", comp.weeks, comp.months)
	}

	b.WriteString("\n--- Replaying the day ---\n")
	switch {
	case replay == nil:
		b.WriteString("(already ran earlier tonight)\n")
	case replay.skipped:
		b.WriteString("(no second read of the day tonight)\n")
	case len(replay.top) == 0:
		b.WriteString("(nothing stood out)\n")
	default:
		for _, p := range replay.top {
			fmt.Fprintf(&b, "%s: %s\n", p.thread, strings.Join(p.facts, "; "))
		}
	}

	if len(notes) > 0 {
		b.WriteString("\n--- Notes ---\n")
		for _, n := range notes {
			b.WriteString(n)
			b.WriteString("\n")
		}
	}
	return b.String()
}
