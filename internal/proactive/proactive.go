// Package proactive is the daemon's initiative seam: a per-minute scheduler that, once per day, writes Ora's diary after the evening close hour and delivers a short brief when the user first shows up after the morning hour. Everything here is best-effort — a failed store read or brain call is logged and simply retried on the next tick, never fatal.
package proactive

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
)

// activityWindow is how recent the newest episode must be for the user to count as present at the keyboard. The brief waits for this so it lands when the user actually sits down, not while the machine idles overnight.
const activityWindow = 10 * time.Minute

// briefMinutesWindow is how far back the brief looks for meeting minutes whose action items may still be open.
const briefMinutesWindow = 3 * 24 * time.Hour

// dayFormat is the local calendar-day key used for diary rows.
const dayFormat = "2006-01-02"

// Scheduler owns the two daily proactive duties. Construct with New and run with Run; both duties key their once-per-day state off the diary table itself (the close is done when today's kind='day' row exists, the brief when today's kind='brief' row does), so a daemon restart never repeats or loses a delivery.
type Scheduler struct {
	store  *db.Store
	brain  brain.Brain
	notify func(title, body string)
	// briefHour and closeHour are local hours; negative disables that seam. Resolved from config in New.
	briefHour int
	closeHour int
	// now is the clock, replaceable in tests. Day boundaries and hour checks are all local time.
	now func() time.Time
}

// New builds a Scheduler from the store, a one-shot brain, a desktop-notification func (NotifySend in production), and the proactive config, whose zero hours resolve to the defaults.
func New(store *db.Store, b brain.Brain, notify func(title, body string), cfg config.ProactiveConfig) *Scheduler {
	briefHour, closeHour := cfg.Hours()
	return &Scheduler{store: store, brain: b, notify: notify, briefHour: briefHour, closeHour: closeHour, now: time.Now}
}

// Run checks the brief and close conditions once a minute until ctx ends. A condition that is not yet met, or a duty that failed, is simply re-checked on the next tick.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.tick(ctx)
		case <-ctx.Done():
			slog.Debug("proactive scheduler stopped")
			return
		}
	}
}

// tick runs both duties' condition checks once. Split from Run so tests can drive the schedule directly.
func (s *Scheduler) tick(ctx context.Context) {
	s.maybeBrief(ctx)
	s.maybeClose(ctx)
}

// maybeClose writes the day's diary entry once the close hour has passed, provided today has seen any activity at all and no entry exists yet. The entry's existence is the done-marker, so a daemon started after the close hour still closes the day.
func (s *Scheduler) maybeClose(ctx context.Context) {
	if s.closeHour < 0 {
		return
	}
	now := s.now()
	if now.Hour() < s.closeHour {
		return
	}
	day := now.Format(dayFormat)
	existing, err := s.store.DiaryEntry(ctx, day, "day")
	if err != nil {
		slog.Warn("evening close: reading today's diary entry failed", "error", err)
		return
	}
	if existing != "" {
		return
	}
	// Some activity must have happened today — a machine that sat powered off or idle all day has no day to write about. Episodes are stored in UTC; the day comparison is local.
	last, err := s.store.MemoryAsOf(ctx, "episode:recent")
	if err != nil || last.IsZero() || last.Local().Format(dayFormat) != day {
		return
	}
	if err := s.closeDay(ctx, now, day); err != nil {
		slog.Warn("evening close failed, retrying next tick", "error", err)
	}
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
	s.notify("Day's written down", firstLine(entry))
	return nil
}

// diaryInstruction is the head of the evening diary prompt; composeDiaryPrompt appends the day's material after it.
const diaryInstruction = `You are Ora, an ambient companion that watches the user's day through their screen and keeps a private diary in its own voice. Write today's entry, in the first person, about the user's day.

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
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
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
	yesterday, err := s.store.DiaryEntry(ctx, now.AddDate(0, 0, -1).Format(dayFormat), "day")
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
		fmt.Fprintf(&b, "%s — %s\n", w.CreatedAt.Local().Format("15:04"), summaryText(w.Content))
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
const understandingInstruction = `You are Ora. Below are your current understanding of the user — your standing model of who they are, what they are working toward, their habits, and the people around them — and today's diary entry. Rewrite the understanding so it stays current.

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

// maybeBrief delivers the morning brief once the brief hour has passed and the user's first activity of the day shows up — the newest episode inside activityWindow is that signal. Today's kind='brief' diary row is the once-per-day marker.
func (s *Scheduler) maybeBrief(ctx context.Context) {
	if s.briefHour < 0 {
		return
	}
	now := s.now()
	if now.Hour() < s.briefHour {
		return
	}
	day := now.Format(dayFormat)
	existing, err := s.store.DiaryEntry(ctx, day, "brief")
	if err != nil {
		slog.Warn("morning brief: reading brief marker failed", "error", err)
		return
	}
	if existing != "" {
		return
	}
	last, err := s.store.MemoryAsOf(ctx, "episode:recent")
	if err != nil || last.IsZero() || s.now().Sub(last) > activityWindow {
		return
	}
	if err := s.deliverBrief(ctx, now, day); err != nil {
		slog.Warn("morning brief failed, retrying next tick", "error", err)
	}
}

// briefInstruction heads the morning brief prompt. The brief is a notification, so brevity is the whole design.
const briefInstruction = `You are Ora, writing the user's morning brief. It is delivered as a desktop notification, so it must read in a few seconds.

Principles:
- At most three short lines, plain text, no markdown, no greeting, no sign-off.
- Draw only on the material below; never invent items.
- Lead with whatever most needs the user's attention today: unfinished action items first, then live threads that moved or stalled, then anything from yesterday that carries into today.`

// deliverBrief composes the brief from live threads, recent meeting minutes, and yesterday's diary entry, asks the brain for it, records the kind='brief' marker, and posts the notification. The marker is written before the notification so a failure after the brain call can at worst lose one notification, never repeat one every minute.
func (s *Scheduler) deliverBrief(ctx context.Context, now time.Time, day string) error {
	threads, err := s.store.GetLiveThreads(ctx, 8)
	if err != nil {
		return err
	}
	minutes, err := s.store.NotesOfKindSince(ctx, "meeting", now.Add(-briefMinutesWindow))
	if err != nil {
		return err
	}
	yesterday, err := s.store.DiaryEntry(ctx, now.AddDate(0, 0, -1).Format(dayFormat), "day")
	if err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString(briefInstruction)
	fmt.Fprintf(&b, "\n\nToday is %s.\n", now.Format("Monday, 2 January 2006"))

	b.WriteString("\n--- Live threads ---\n")
	if len(threads) == 0 {
		b.WriteString("(none)\n")
	}
	for _, t := range threads {
		if t.State != "" {
			fmt.Fprintf(&b, "%s (%s): %s\n", t.Subject, t.Kind, t.State)
		} else {
			fmt.Fprintf(&b, "%s (%s)\n", t.Subject, t.Kind)
		}
	}

	b.WriteString("\n--- Meeting minutes from the last few days ---\n")
	writeNotes(&b, minutes)

	b.WriteString("\n--- Yesterday's diary entry ---\n")
	writeOrNone(&b, yesterday)

	brief, err := s.brain(ctx, b.String())
	if err != nil {
		return fmt.Errorf("brief brain call: %w", err)
	}
	brief = strings.TrimSpace(brief)
	if brief == "" {
		return fmt.Errorf("brief brain call returned nothing")
	}
	if err := s.store.SetDiaryEntry(ctx, day, "brief", brief); err != nil {
		return err
	}
	s.notify("Morning brief", brief)
	return nil
}

// summaryText pulls the prose out of a summary node's content: task summaries are stored as marshalled JSON (see db.LogSemanticNode) and the model should read the sentence, not the blob. Digests and anything non-JSON pass through unchanged.
func summaryText(content string) string {
	var t struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(content), &t); err == nil && strings.TrimSpace(t.Summary) != "" {
		return t.Summary
	}
	return content
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

// firstLine returns the first non-empty line of s — the diary prompt asks for a standalone salient first sentence exactly so the close notification can be this.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// NotifySend posts a desktop notification through notify-send, which GNOME provides — the recorder has its own twin with a microphone icon. Failure is logged and ignored: a missed notification must never break the seam that sent it.
func NotifySend(title, body string) {
	if err := exec.Command("notify-send", "-a", "Ora", "-i", "x-office-calendar", title, body).Run(); err != nil {
		slog.Debug("notify-send failed", "title", title, "error", err)
	}
}
