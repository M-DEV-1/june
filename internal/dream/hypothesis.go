package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"june/internal/db"
	"june/internal/util"
)

// stageReport is what the hypothesis stage hands the morning report: counts plus the sentences worth repeating.
type stageReport struct {
	tested, promoted, retired, adopted int
	lines                              []string
}

// rawVerdict is the judge's JSON for one hypothesis, before Go's mechanics decide what actually happens to the row.
type rawVerdict struct {
	ID         int64  `json:"id"`
	Verdict    string `json:"verdict"`
	Confidence string `json:"confidence"`
	Evidence   string `json:"evidence"`
	Action     string `json:"action"`
}

// rawHypothesis is the extraction call's JSON for one proposed new hypothesis.
type rawHypothesis struct {
	Statement  string `json:"statement"`
	Confidence string `json:"confidence"`
}

// hypStage tests every open hypothesis against the week's evidence and adopts new ones from the dailies, committing everything (and the stage token) in one transaction. A brain transport error aborts the stage for this wake; a reply that never parses skips the judging but still commits the token, journaled, so the night does not burn calls retrying.
func (r *Runner) hypStage(ctx context.Context, night string, fallback bool) (stageReport, error) {
	var rep stageReport

	open, err := r.store.OpenHypotheses(ctx)
	if err != nil {
		return rep, err
	}
	ev, err := r.evidenceMaterial(ctx, night, fallback)
	if err != nil {
		return rep, err
	}

	var verdicts []db.HypothesisVerdict
	judged := map[int64]bool{}
	if len(open) > 0 {
		var raw []rawVerdict
		err := r.askJSON(ctx, night, "verdicts", verdictPrompt(open, ev.full), &raw)
		switch {
		case errors.Is(err, errUnparsable):
			rep.lines = append(rep.lines, "The judge's verdicts never parsed as JSON, so no hypotheses were tested tonight.")
		case err != nil:
			return rep, err
		default:
			byID := make(map[int64]db.Hypothesis, len(open))
			for _, h := range open {
				byID[h.ID] = h
			}
			for _, v := range validVerdicts(raw, byID) {
				update, note := decide(byID[v.ID], v, night)
				verdicts = append(verdicts, update)
				judged[v.ID] = true
				rep.tested++
				switch update.Status {
				case "promoted":
					rep.promoted++
					rep.lines = append(rep.lines, fmt.Sprintf("Promoted: %s (%s).", byID[v.ID].Statement, update.Reason))
				case "retired":
					rep.retired++
					rep.lines = append(rep.lines, fmt.Sprintf("Retired: %s (%s).", byID[v.ID].Statement, update.Reason))
				}
				if note != "" {
					rep.lines = append(rep.lines, note)
				}
			}
		}
	}

	// The stale mechanic is Go's alone: an open hypothesis the judge never reached tonight, thirty days old and never once tested, is dead weight and retires without a verdict.
	for _, h := range open {
		if judged[h.ID] || h.TimesTested > 0 || h.Born > nightMinus(night, staleAfterDays) {
			continue
		}
		verdicts = append(verdicts, db.HypothesisVerdict{ID: h.ID, Confidence: h.Confidence, Status: "retired", Reason: fmt.Sprintf("open %d days and never tested", staleAfterDays)})
		rep.retired++
		rep.lines = append(rep.lines, fmt.Sprintf("Retired: %s (open %d days and never tested).", h.Statement, staleAfterDays))
	}

	var adopted []db.NewHypothesis
	if ev.haveDailies {
		var raw []rawHypothesis
		err := r.askJSON(ctx, night, "extract", extractPrompt(open, ev.diary), &raw)
		switch {
		case errors.Is(err, errUnparsable):
			rep.lines = append(rep.lines, "The extraction reply never parsed as JSON, so no new hypotheses were adopted.")
		case err != nil:
			return rep, err
		default:
			adopted = validNew(raw)
			rep.adopted = len(adopted)
			for _, a := range adopted {
				rep.lines = append(rep.lines, fmt.Sprintf("Adopted: %s.", a.Statement))
			}
		}
	}

	return rep, r.store.CommitHypothesisStage(ctx, night, verdicts, adopted)
}

// evidence is the grounded material the hypothesis stage assembles: full carries every section for the judging call, diary only the diary entries (and the fallback summaries) for the extraction call, and haveDailies whether extraction has anything to mine.
type evidence struct {
	full        string
	diary       string
	haveDailies bool
}

// evidence section indices, in the order the material renders them.
const (
	secDiary = iota
	secWork
	secThreads
	secMeetings
	secFallback
	secCount
)

// evidenceHeaders label the sections; the fallback header only renders on a night whose diary entry never arrived.
var evidenceHeaders = [secCount]string{"Diary:", "The week's work:", "Ongoing threads:", "Meetings:", "Today's screen summaries (no diary entry was written tonight):"}

// evidenceItem is one datable block of evidence, tagged with its section so the budget can drop the oldest items across all sections while the rendering keeps them grouped.
type evidenceItem struct {
	section int
	at      time.Time
	text    string
}

// evidenceMaterial assembles the grounded evidence the hypothesis calls read: the last week of diary entries, the week's work summaries, the standing active threads, and the week's meeting minutes, all budget-capped with the oldest items truncated first. The fallback section (the night's raw summaries when no diary entry was written) rides along for both calls, as before.
func (r *Runner) evidenceMaterial(ctx context.Context, night string, fallback bool) (evidence, error) {
	var ev evidence
	now := r.now()
	var items []evidenceItem

	back := r.sweepBack(ctx, night)
	dailies, err := r.store.DiaryDays(ctx, nightMinus(night, back), night)
	if err != nil {
		return ev, err
	}
	ev.haveDailies = len(dailies) > 0
	for _, d := range dailies {
		at, _ := time.ParseInLocation(time.DateOnly, d.Day, time.Local)
		items = append(items, evidenceItem{secDiary, at, fmt.Sprintf("--- Diary entry, %s ---\n%s\n", d.Day, d.Content)})
	}

	work, err := r.store.SummaryTimeline(ctx, now.AddDate(0, 0, -(back+1)), now)
	if err != nil {
		return ev, err
	}
	for _, w := range work {
		line, ok := workLine(w)
		if !ok {
			continue
		}
		items = append(items, evidenceItem{secWork, w.CreatedAt, line + "\n"})
	}

	threads, err := r.store.ActiveThreads(ctx, evidenceThreads)
	if err != nil {
		return ev, err
	}
	for _, t := range threads {
		line := t.Subject
		if strings.TrimSpace(t.State) != "" {
			line += " — " + t.State
		}
		items = append(items, evidenceItem{secThreads, t.LastSeen, line + "\n"})
	}

	meetings, err := r.store.NotesOfKindSince(ctx, "meeting", now.AddDate(0, 0, -7))
	if err != nil {
		return ev, err
	}
	for _, m := range meetings {
		items = append(items, evidenceItem{secMeetings, m.CreatedAt, fmt.Sprintf("--- Meeting, %s ---\n%s\n", m.CreatedAt.Local().Format("Jan 2"), meetingEvidenceBody(m.Content))})
	}

	if fallback {
		start := r.windowStart(night)
		dayStart := db.DayStart(start)
		summaries, err := r.store.SummaryTimeline(ctx, dayStart, now)
		if err != nil {
			return ev, err
		}
		for _, w := range summaries {
			items = append(items, evidenceItem{secFallback, w.CreatedAt, fmt.Sprintf("%s — %s\n", w.CreatedAt.Local().Format("15:04"), db.SummaryText(w.Content))})
		}
	}

	kept := capEvidence(items, evidenceBudget)
	ev.full = renderEvidence(kept, fallback, nil)
	ev.diary = renderEvidence(kept, fallback, map[int]bool{secDiary: true, secFallback: true})
	return ev, nil
}

// capEvidence keeps items within the byte budget by dropping the oldest first, then restores the original per-section order. Input order within a section must be chronological, which every source query already guarantees.
func capEvidence(items []evidenceItem, budget int) []evidenceItem {
	total := 0
	for _, it := range items {
		total += len(it.text)
	}
	if total <= budget {
		return items
	}
	// Sort a copy of the indices newest first and keep from the top until the budget runs out, so what survives is exactly the newest material.
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return items[b].at.Compare(items[a].at) })
	keep := make([]bool, len(items))
	used := 0
	for _, i := range order {
		if used+len(items[i].text) > budget {
			continue
		}
		used += len(items[i].text)
		keep[i] = true
	}
	var out []evidenceItem
	for i, it := range items {
		if keep[i] {
			out = append(out, it)
		}
	}
	return out
}

// renderEvidence lays the kept items out under their section headers, in section order. only, when non-nil, restricts which sections render — the extraction call's diary-only view. A section with nothing left still prints "(none)" so the model knows absence from padding.
func renderEvidence(items []evidenceItem, fallback bool, only map[int]bool) string {
	var b strings.Builder
	for sec := 0; sec < secCount; sec++ {
		if only != nil && !only[sec] {
			continue
		}
		if sec == secFallback && !fallback {
			continue
		}
		fmt.Fprintf(&b, "\n%s\n", evidenceHeaders[sec])
		empty := true
		for _, it := range items {
			if it.section != sec {
				continue
			}
			b.WriteString(it.text)
			empty = false
		}
		if empty {
			b.WriteString("(none)\n")
		}
	}
	return b.String()
}

// workLine renders one summary node as a single line for the week's-work section, with ok false for the compiler's "Raw Activity Log" fallback buckets — noise, not a stretch of work.
func workLine(w db.WindowSummary) (string, bool) {
	var t struct {
		Task    string `json:"task_name"`
		Summary string `json:"summary"`
	}
	text := w.Content
	if err := json.Unmarshal([]byte(w.Content), &t); err == nil && t.Task != "" {
		if t.Task == "Raw Activity Log" {
			return "", false
		}
		text = t.Task + " — " + t.Summary
	}
	return w.CreatedAt.Local().Format("Jan 2 15:04") + " " + util.OneLine(text), true
}

// headLines returns the first n lines of s, which is how much of one meeting's minutes the evidence carries.
// meetingEvidenceBody is one meeting's minutes as the evidence carries them: whole.
// They used to be cut to their leading 40 lines, which on a real 48-line minutes file reached Attendees, Key points and Decisions and dropped Action items off the end — so the judge read what was discussed and never what anyone agreed to do. evidenceBudget already bounds the assembly by dropping whole items oldest-first, which is the right shape for this: a meeting is either carried or it is not, never carried headless.
func meetingEvidenceBody(s string) string {
	return strings.TrimRight(s, "\n")
}

// validVerdicts keeps the structurally sound verdicts: a known id (once each), enum-valid verdict, confidence and action, evidence truncated to its cap. Everything else is dropped and logged.
func validVerdicts(raw []rawVerdict, open map[int64]db.Hypothesis) []rawVerdict {
	var out []rawVerdict
	seen := map[int64]bool{}
	for _, v := range raw {
		_, known := open[v.ID]
		if !known || seen[v.ID] ||
			!slices.Contains([]string{"supported", "contradicted", "unclear"}, v.Verdict) ||
			!slices.Contains([]string{"low", "medium", "high"}, v.Confidence) ||
			!slices.Contains([]string{"keep", "promote", "retire"}, v.Action) {
			slog.Warn("dreaming: dropping an invalid verdict", "id", v.ID, "verdict", v.Verdict, "confidence", v.Confidence, "action", v.Action)
			continue
		}
		seen[v.ID] = true
		if len(v.Evidence) > maxEvidenceLen {
			v.Evidence = v.Evidence[:maxEvidenceLen]
		}
		out = append(out, v)
	}
	return out
}

// validNew keeps at most maxNewHypotheses proposals with a non-empty statement under the length cap; confidence defaults to low when not enum-valid. Duplicates of existing statements are handled by the table's UNIQUE index, not here.
func validNew(raw []rawHypothesis) []db.NewHypothesis {
	var out []db.NewHypothesis
	for _, h := range raw {
		s := strings.TrimSpace(h.Statement)
		if s == "" || len(s) > maxStatementLen {
			slog.Warn("dreaming: dropping an invalid new hypothesis", "len", len(s))
			continue
		}
		if !slices.Contains([]string{"low", "medium", "high"}, h.Confidence) {
			h.Confidence = "low"
		}
		out = append(out, db.NewHypothesis{Statement: s, Confidence: h.Confidence})
		if len(out) == maxNewHypotheses {
			break
		}
	}
	return out
}

// decide applies the mechanics to one judged hypothesis. The model recommends; Go decides: promotion needs promoteAfterTests tests and promoteMinAgeDays of age, a retireContradictions-th contradiction retires regardless of the recommendation, and everything else stays open with the judge's confidence. The returned note, when non-empty, is a journal line for the morning report.
func decide(h db.Hypothesis, v rawVerdict, night string) (db.HypothesisVerdict, string) {
	update := db.HypothesisVerdict{
		ID:           h.ID,
		Confidence:   v.Confidence,
		Status:       "open",
		LastTested:   night,
		EvidenceLine: fmt.Sprintf("[%s] %s — %s", night, v.Verdict, v.Evidence),
		Tested:       true,
	}
	contradictions := strings.Count(h.Evidence, contradictedMark)
	if v.Verdict == "contradicted" {
		contradictions++
	}
	tests := h.TimesTested + 1
	note := ""
	switch {
	case contradictions >= retireContradictions:
		update.Status, update.Reason = "retired", fmt.Sprintf("contradicted %d times", contradictions)
	case v.Action == "retire":
		update.Status, update.Reason = "retired", "retired by the nightly judge"
	case v.Action == "promote":
		if tests >= promoteAfterTests && h.Born <= nightMinus(night, promoteMinAgeDays) {
			update.Status, update.Reason = "promoted", fmt.Sprintf("promoted after %d tests", tests)
		} else {
			note = fmt.Sprintf("Promotion refused for %q: %d tests, born %s.", h.Statement, tests, h.Born)
		}
	}
	return update, note
}
