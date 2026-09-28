package proactive

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"june/internal/db"
	"june/internal/util"
)

// maybeClose writes the day's diary entry once the close hour has passed, provided the day has seen any activity at all and no entry exists yet. The entry's existence is the done-marker, so a daemon started after the close hour still closes the day.
// Yesterday is checked first, because a machine asleep at yesterday's close hour never got yesterday's row and the day is otherwise lost for good — it is also today's "yesterday's entry" prompt input and the dream's material.
func (s *Scheduler) maybeClose(ctx context.Context) {
	if s.closeHour < 0 {
		return
	}
	now := s.now()
	if now.Hour() < s.closeHour {
		return
	}
	// One second before today began: the timeline it composes covers the whole of yesterday and the prompt's date line names yesterday.
	// A deadline each, because each close makes two brain calls: sharing one deadline meant four calls had to fit in it on the first tick after a night the machine slept through, and yesterday's close ate the time today's needed.
	yesterday := db.DayStart(now).Add(-time.Second)
	s.duty(ctx, func(ctx context.Context) { s.closeOneDay(ctx, "close-yesterday", yesterday) })
	s.duty(ctx, func(ctx context.Context) { s.closeOneDay(ctx, "close", now) })
}

// closeOneDay writes one calendar day's diary entry if it has none and that day saw any activity. Input: the tick's context, the backoff key the duty is tracked under, and the moment the day is written from — the tick's own now for today, the last second of yesterday when catching up a day the machine slept through. Output: nothing; a failure is logged and backed off.
func (s *Scheduler) closeOneDay(ctx context.Context, duty string, at time.Time) {
	if s.backedOff(duty, s.now()) {
		return
	}
	day := at.Format(time.DateOnly)
	existing, err := s.store.DiaryEntry(ctx, day, "day")
	if err != nil {
		slog.Warn("evening close: reading the day's diary entry failed", "day", day, "error", err)
		return
	}
	if existing != "" {
		return
	}
	if !s.hadActivity(ctx, at) {
		return
	}
	if err := s.closeDay(ctx, at, day); err != nil {
		s.failed(duty, s.now())
		slog.Warn("evening close failed, backing off", "day", day, "error", err)
		return
	}
	s.succeeded(duty)
}

// hadActivity reports whether the machine was in use on one local day — a day that sat powered off or idle has no day to write about. Input: a moment on that day. Output: true when the newest episode falls on it, or when the day recorded any screen summary at all. The newest episode only ever answers for the last day the machine was awake, so a day being caught up after a sleep is answered from its own timeline instead. Episodes are stored in UTC; the day comparison is local.
func (s *Scheduler) hadActivity(ctx context.Context, at time.Time) bool {
	last, err := s.store.MemoryAsOf(ctx, "episode:recent")
	if err == nil && !last.IsZero() && last.Local().Format(time.DateOnly) == at.Format(time.DateOnly) {
		return true
	}
	summaries, err := s.store.SummaryTimeline(ctx, db.DayStart(at), at)
	return err == nil && len(summaries) > 0
}

// closeDay composes the day's material, asks the brain for the diary entry and the rewritten understanding doc, persists both, and posts the close notification. The kind='day' row is written last: it is the once-per-day marker, so nothing can half-complete and still count as done.
func (s *Scheduler) closeDay(ctx context.Context, now time.Time, day string) error {
	prompt, err := s.composeDiaryPrompt(ctx, now)
	if err != nil {
		return err
	}
	entry, err := s.brain(ctx, prompt)
	if err != nil {
		return fmt.Errorf("diary brain call: %w", err)
	}
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return fmt.Errorf("diary brain call returned nothing")
	}

	understanding, err := s.store.DiaryEntry(ctx, "", "understanding")
	if err != nil {
		return err
	}
	rewritten, err := s.brain(ctx, composeUnderstandingPrompt(understanding, entry))
	if err != nil {
		return fmt.Errorf("understanding brain call: %w", err)
	}
	if u := strings.TrimSpace(rewritten); u != "" {
		if err := s.store.SetDiaryEntry(ctx, "", "understanding", u); err != nil {
			return err
		}
	}
	if err := s.store.SetDiaryEntry(ctx, day, "day", entry); err != nil {
		return err
	}
	s.say(Notice{Title: "Day's written down", Body: util.FirstLine(entry), Place: "days", ID: day, Kind: "close", Actions: noticeActions})
	return nil
}

// diaryInstruction is the head of the evening diary prompt; composeDiaryPrompt appends the day's material after it.
const diaryInstruction = `You are June, an ambient companion that watches the user's day through their screen and keeps a private diary in its own voice. Write today's entry, in the first person, about the user's day.

Principles:
- Ground everything in the material below. If the material does not say it, the entry does not say it — no invented events, names, times, or motives.
- Open with a single sentence naming the most salient thing about today. It stands alone as the first line and doubles as the close notification, so it must carry on its own.
- Then say what happened, briefly and in rough order.
- Then say what you noticed: patterns, anomalies, anything that differs from your current understanding of the user.
- Then a section labelled "Hypotheses:" — each on its own line, each ending with one confidence word in parentheses: certain, likely, tentative, or guessing. Only hypotheses today's material actually supports.
- Then a section labelled "Open questions:" — what you could not settle from what you saw.
- Plain prose, no markdown, under about 400 words in total.`

// composeDiaryPrompt assembles the diary brain call's input: the instruction, then today's summary timeline, today's meeting minutes, the current understanding doc, yesterday's entry, and the personal context. Every section is always present, "(none)" when empty, so the model never guesses whether material was withheld or just absent.
func (s *Scheduler) composeDiaryPrompt(ctx context.Context, now time.Time) (string, error) {
	dayStart := db.DayStart(now)
	summaries, err := s.store.SummaryTimeline(ctx, dayStart, now)
	if err != nil {
		return "", err
	}
	minutes, err := s.store.NotesOfKindSince(ctx, "meeting", dayStart)
	if err != nil {
		return "", err
	}
	understanding, err := s.store.DiaryEntry(ctx, "", "understanding")
	if err != nil {
		return "", err
	}
	yesterday, err := s.store.DiaryEntry(ctx, now.AddDate(0, 0, -1).Format(time.DateOnly), "day")
	if err != nil {
		return "", err
	}
	personal, err := s.store.PersonalContext(ctx)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(diaryInstruction)
	fmt.Fprintf(&b, "\n\nToday is %s.\n", now.Format("Monday, 2 January 2006"))

	b.WriteString("\n--- Today's timeline, from the screen summaries ---\n")
	if len(summaries) == 0 {
		b.WriteString("(none)\n")
	}
	for _, w := range summaries {
		fmt.Fprintf(&b, "%s — %s\n", w.CreatedAt.Local().Format("15:04"), db.SummaryText(w.Content))
	}

	b.WriteString("\n--- Meeting minutes written today ---\n")
	writeNotes(&b, minutes)

	b.WriteString("\n--- Your current understanding of the user ---\n")
	writeOrNone(&b, understanding)

	b.WriteString("\n--- Yesterday's diary entry ---\n")
	writeOrNone(&b, yesterday)

	b.WriteString("\n--- Personal context (user-stated, certain) ---\n")
	if len(personal) == 0 {
		b.WriteString("(none)\n")
	}
	for _, p := range personal {
		fmt.Fprintf(&b, "%s: %s\n", p.Subject, p.Content)
	}
	return b.String(), nil
}

// understandingInstruction heads the second, short close call that rewrites the bounded model-of-the-user document in place.
const understandingInstruction = `You are June. Below are your current understanding of the user — your standing model of who they are, what they are working toward, their habits, and the people around them — and today's diary entry. Rewrite the understanding so it stays current.

Principles:
- Fold in only durable shifts: things that will still be true and still matter weeks from now. A one-day event does not belong unless it changed something lasting.
- Keep everything that is still true, correct what today contradicted, and cut what has expired.
- Declarative plain prose, under 300 words.
- Output only the rewritten understanding — no preamble, no commentary.`

// composeUnderstandingPrompt joins the instruction, the current understanding doc (which may be empty on the very first close), and today's entry.
func composeUnderstandingPrompt(understanding, entry string) string {
	var b strings.Builder
	b.WriteString(understandingInstruction)
	b.WriteString("\n\n--- Current understanding ---\n")
	writeOrNone(&b, understanding)
	b.WriteString("\n--- Today's diary entry ---\n")
	b.WriteString(entry)
	b.WriteString("\n")
	return b.String()
}

// writeNotes renders each note's content as its own paragraph, "(none)" when there are none.
func writeNotes(b *strings.Builder, notes []db.Note) {
	if len(notes) == 0 {
		b.WriteString("(none)\n")
		return
	}
	for _, n := range notes {
		b.WriteString(n.Content)
		b.WriteString("\n\n")
	}
}

// writeOrNone writes s followed by a newline, or "(none)" when s is empty.
func writeOrNone(b *strings.Builder, s string) {
	if strings.TrimSpace(s) == "" {
		b.WriteString("(none)\n")
		return
	}
	b.WriteString(s)
	b.WriteString("\n")
}
