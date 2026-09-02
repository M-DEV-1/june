// Package proactive is the daemon's initiative seam: a per-minute scheduler that, once per day, writes Ora's diary after the evening close hour and delivers a short brief when the user first shows up after the morning hour. Everything here is best-effort — a failed store read or brain call is logged and simply retried on the next tick, never fatal.
package proactive

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"ora/internal/brain"
	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/memory"
)

// activityWindow is how recent the newest episode must be for the user to count as present at the keyboard. The brief waits for this so it lands when the user actually sits down, not while the machine idles overnight.
const activityWindow = 10 * time.Minute

// briefMinutesWindow is how far back the brief looks for meeting minutes whose action items may still be open.
const briefMinutesWindow = 3 * 24 * time.Hour

// dayFormat is the local calendar-day key used for diary rows.
const dayFormat = "2006-01-02"

// Store is the slice of *db.Store the scheduler needs: the diary rows both duties key their once-per-day state off, the action items the brief chases, and the day's material the evening close writes from. Declared here rather than taking *db.Store whole, so this package states its entire data dependency in one place and widening it is a deliberate edit instead of an accident.
type Store interface {
	// Both duties key their once-per-day state off the diary table itself, which is why a daemon restart never repeats or loses a delivery.
	DiaryEntry(ctx context.Context, day, kind string) (string, error)
	SetDiaryEntry(ctx context.Context, day, kind, content string) error

	// The open action items the brief names, and the status/priority edits a one-click answer applies to one.
	OpenActionItems(ctx context.Context) ([]memory.ActionItem, error)
	SetActionStatus(ctx context.Context, noteID int64, status string) error
	SetActionPriority(ctx context.Context, noteID int64, priority string) error

	// The day's material the close writes from, plus the store's own notion of "now".
	SummaryTimeline(ctx context.Context, since, until time.Time) ([]db.WindowSummary, error)
	GetLiveThreads(ctx context.Context, limit int) ([]memory.Thread, error)
	NotesOfKindSince(ctx context.Context, kind string, since time.Time) ([]db.Note, error)
	PersonalContext(ctx context.Context) ([]db.PersonalEntry, error)
	MemoryAsOf(ctx context.Context, source string) (time.Time, error)
}

// Scheduler owns the two daily proactive duties. Construct with New and run with Run; both duties key their once-per-day state off the diary table itself (the close is done when today's kind='day' row exists, the brief when today's kind='brief' row does), so a daemon restart never repeats or loses a delivery.
type Scheduler struct {
	// ask posts the one-click question about a stale action item and blocks until the user answers. Nil disables asking.
	ask    func(title, body string, actions []string) (string, error)
	store  Store
	brain  brain.Brain
	notify func(title, body string)
	// briefHour and closeHour are local hours; negative disables that seam. Resolved from config in New.
	briefHour int
	closeHour int
	// now is the clock, replaceable in tests. Day boundaries and hour checks are all local time.
	now func() time.Time
	// weeklyStudy runs the Sunday-only weekly system log + distillation study pass; unset (the zero value) disables the trigger entirely. See SetWeeklyStudy.
	weeklyStudy func(ctx context.Context, now time.Time) error
}

// New builds a Scheduler from the store, a one-shot brain, a desktop-notification func (NotifySend in production), and the proactive config, whose zero hours resolve to the defaults.
func New(store Store, b brain.Brain, notify func(title, body string), cfg config.ProactiveConfig) *Scheduler {
	briefHour, closeHour := cfg.Hours()
	return &Scheduler{store: store, brain: b, notify: notify, briefHour: briefHour, closeHour: closeHour, now: time.Now}
}

// SetAsk wires the one-click question the brief asks about an item that has gone quiet. Unset, the brief still names stale items in its text and simply never asks about one — cmd/daemon.go is the only production caller. The func posts the question and blocks until the user answers or dismisses it, returning the action they chose ("" for a dismissal).
func (s *Scheduler) SetAsk(fn func(title, body string, actions []string) (string, error)) {
	s.ask = fn
}

// SetWeeklyStudy wires the Sunday-only weekly system log + distillation study pass — cmd/daemon.go is the only production caller. Unset, the trigger never fires.
func (s *Scheduler) SetWeeklyStudy(fn func(ctx context.Context, now time.Time) error) {
	s.weeklyStudy = fn
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
	s.maybeWeeklyStudy(ctx)
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
- Lead with whatever most needs the user's attention today: open action items first, then live threads that moved or stalled, then anything from yesterday that carries into today.
- Items under "ask how these are going" have been owed for a while. Do not restate them as news. Ask about one, in the user's own terms, so they can answer and you can update it.`

// deliverBrief composes the brief from live threads, recent meeting minutes, and yesterday's diary entry, asks the brain for it, records the kind='brief' marker, and posts the notification. The marker is written before the notification so a failure after the brain call can at worst lose one notification, never repeat one every minute.
func (s *Scheduler) deliverBrief(ctx context.Context, now time.Time, day string) error {
	threads, err := s.store.GetLiveThreads(ctx, 8)
	if err != nil {
		return err
	}
	actions, err := s.store.OpenActionItems(ctx)
	if err != nil {
		return err
	}
	owed, ask := partitionActions(actions, now)
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

	b.WriteString("\n--- Open action items ---\n")
	writeActions(&b, owed)

	b.WriteString("\n--- Owed for a while; ask how these are going ---\n")
	writeActions(&b, ask)

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
	// Only the user's own work is theirs to report progress on. Somebody else's item still shows in the brief — they are waiting on it — but being asked "any progress?" about a task another person owes is a question they cannot answer.
	if mine := firstOwnedByUser(ask, s.identityText(ctx)); mine != nil {
		go s.askAbout(ctx, *mine)
	}
	return nil
}

// The answers the stale-item question offers, in the order they are shown. The keys are what notify-send prints back; the labels are what the user reads.
var askAnswers = []struct{ key, label, status, priority string }{
	{"done", "Done", memory.StatusDone, ""},
	{"dropped", "Not happening", memory.StatusDropped, ""},
	{"low", "Not urgent", "", memory.PriorityLow},
}

// identityText returns the personal-context entry naming the user, or "" when there is none.
func (s *Scheduler) identityText(ctx context.Context) string {
	entries, err := s.store.PersonalContext(ctx)
	if err != nil {
		slog.Debug("could not read who the user is", "error", err)
		return ""
	}
	for _, e := range entries {
		if e.Subject == "identity" {
			return e.Content
		}
	}
	return ""
}

// firstOwnedByUser returns the first item the user themselves owes, or nil when none of them is theirs.
func firstOwnedByUser(items []memory.ActionItem, identity string) *memory.ActionItem {
	for _, a := range items {
		if memory.OwnedByUser(a.Owner, identity) {
			return &a
		}
	}
	return nil
}

// askAbout puts one stale action item to the user as a notification they can answer with a click, and applies whatever they choose. Dismissing it changes nothing and the item is simply asked about again another morning. Runs in its own goroutine because the notification blocks until it is answered, which can be hours.
//
// ponytail: re-asks every morning until answered. If that grates, stamp the item with the day it was last asked about and leave a gap.
func (s *Scheduler) askAbout(ctx context.Context, a memory.ActionItem) {
	if s.ask == nil {
		return
	}
	labels := make([]string, 0, len(askAnswers))
	for _, ans := range askAnswers {
		labels = append(labels, ans.key+"="+ans.label)
	}
	body := fmt.Sprintf("%s — %s\n\nOpen since %s. Any progress?", a.Owner, a.Text, a.Raised.Format(dayFormat))
	chosen, err := s.ask("Still open", body, labels)
	if err != nil {
		slog.Debug("could not ask about a stale action item", "note_id", a.NoteID, "error", err)
		return
	}
	for _, ans := range askAnswers {
		if ans.key != chosen {
			continue
		}
		if ans.status != "" {
			if err := s.store.SetActionStatus(ctx, a.NoteID, ans.status); err != nil {
				slog.Warn("could not apply the answer to a stale action item", "note_id", a.NoteID, "error", err)
			}
		}
		if ans.priority != "" {
			if err := s.store.SetActionPriority(ctx, a.NoteID, ans.priority); err != nil {
				slog.Warn("could not apply the answer to a stale action item", "note_id", a.NoteID, "error", err)
			}
		}
		return
	}
}

// NotifySendAsk posts a notification carrying one button per action and blocks until the user picks one or dismisses it, returning the chosen action's key ("" for a dismissal). Each action is "key=Label"; notify-send prints the key of whatever was clicked. --action implies --wait, so this call is as long-lived as the notification on screen.
func NotifySendAsk(title, body string, actions []string) (string, error) {
	args := []string{"-a", "Ora"}
	for _, a := range actions {
		args = append(args, "--action="+a)
	}
	args = append(args, title, body)
	out, err := exec.Command("notify-send", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// maybeWeeklyStudy fires the Sunday-only weekly system log + distillation study pass, once the brief hour has passed and the user's first activity of the day shows up — the same first-activity gate maybeBrief uses, so it rides the Claude workday window rather than firing overnight. Disabled when no weeklyStudy func is wired (SetWeeklyStudy never called) or the brief hour itself is disabled. Today's kind='weekly-study' diary row is the once-per-Sunday marker, written after the attempt regardless of its outcome — a failed pass is logged, not retried every minute for the rest of the day.
func (s *Scheduler) maybeWeeklyStudy(ctx context.Context) {
	if s.weeklyStudy == nil || s.briefHour < 0 {
		return
	}
	now := s.now()
	if now.Weekday() != time.Sunday || now.Hour() < s.briefHour {
		return
	}
	day := now.Format(dayFormat)
	existing, err := s.store.DiaryEntry(ctx, day, "weekly-study")
	if err != nil {
		slog.Warn("weekly study: reading marker failed", "error", err)
		return
	}
	if existing != "" {
		return
	}
	last, err := s.store.MemoryAsOf(ctx, "episode:recent")
	if err != nil || last.IsZero() || s.now().Sub(last) > activityWindow {
		return
	}
	if err := s.weeklyStudy(ctx, now); err != nil {
		slog.Warn("weekly study failed", "error", err)
	}
	if err := s.store.SetDiaryEntry(ctx, day, "weekly-study", "weekly system log and distillation study ran"); err != nil {
		slog.Warn("weekly study: writing marker failed", "error", err)
	}
}

// staleAfter is how long an action item may sit open before the brief stops restating it and starts asking how it is going. Roughly a working week: long enough that repeating it would have already worn out its welcome, short enough that the question still lands while the work is live.
const staleAfter = 5 * 24 * time.Hour

// partitionActions splits the open action items into the ones the brief should state plainly and the ones it should ask about instead. Input: every open item, and the moment the brief is being written. Output: owed, the items to report, and ask, the ones that have been sitting long enough that repeating them has stopped being useful.
//
// A high-priority item is never moved to ask, however old it is: the question is for work that has gone quiet, not for work that matters, and softening "merge the PR" into "how is the PR going" on day six would bury the thing the user most needs to see. Everything else crosses over once it has been open longer than staleAfter.
//
// Order decides what survives, since the brief is three lines: owed leads with the highest priority and, within one priority, the item that has been waiting longest; ask leads with the longest-neglected item, which is the one whose answer is most overdue.
func partitionActions(open []memory.ActionItem, now time.Time) (owed, ask []memory.ActionItem) {
	for _, a := range open {
		// A zero Raised means the note carried no date to read back, not that the work is ancient. Treating it as stale put it in the nagging list for good: now.Sub of a zero time is two millennia, which beats any threshold.
		if a.Raised.IsZero() {
			owed = append(owed, a)
			continue
		}
		if a.Priority != memory.PriorityHigh && now.Sub(a.Raised) > staleAfter {
			ask = append(ask, a)
			continue
		}
		owed = append(owed, a)
	}
	slices.SortStableFunc(owed, func(x, y memory.ActionItem) int {
		if c := priorityRank(x.Priority) - priorityRank(y.Priority); c != 0 {
			return c
		}
		return x.Raised.Compare(y.Raised)
	})
	slices.SortStableFunc(ask, func(x, y memory.ActionItem) int { return x.Raised.Compare(y.Raised) })
	return owed, ask
}

// priorityRank orders the priorities most-pressing first, for sorting.
func priorityRank(p string) int {
	switch p {
	case memory.PriorityHigh:
		return 0
	case memory.PriorityLow:
		return 2
	default:
		return 1
	}
}

// writeActions renders each action item as one line — who owes it, what it is, and how long it has been open — or "(none)" when there are none, so the model never has to guess whether the list was withheld or is simply empty.
func writeActions(b *strings.Builder, items []memory.ActionItem) {
	if len(items) == 0 {
		b.WriteString("(none)\n")
		return
	}
	for _, a := range items {
		fmt.Fprintf(b, "[%s] %s — %s (raised %s)\n", a.Priority, a.Owner, a.Text, a.Raised.Format(dayFormat))
	}
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

// readAction is the notify-send action key for the button that opens a long body in full.
const readAction = "read"

// longBodyRunes is the body length past which GNOME's banner has already cut the text off. GNOME shows a few lines of a notification and hides the rest, which is how every morning brief and meeting prep went unread past its first sentences.
const longBodyRunes = 200

// notifyArgs builds the notify-send arguments for one notification: the app name and icon, then for a body longer than longBodyRunes a "Read in full" action, then the title and body.
func notifyArgs(icon, title, body string) []string {
	args := []string{"-a", "Ora", "-i", icon}
	if len([]rune(body)) > longBodyRunes {
		args = append(args, "--action="+readAction+"=Read in full")
	}
	return append(args, title, body)
}

// Notify posts a desktop notification through notify-send, which GNOME provides. A long body also gets a "Read in full" button that opens the whole text in a zenity window, because GNOME's banner truncates it. Failure is logged and ignored: a missing notification must never take down the work that produced it.
// The long form waits in the background for the button, up to an hour, so the caller never blocks on it.
func Notify(icon, title, body string) {
	args := notifyArgs(icon, title, body)
	if len([]rune(body)) <= longBodyRunes {
		if err := exec.Command("notify-send", args...).Run(); err != nil {
			slog.Debug("notify-send failed", "title", title, "error", err)
		}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		out, err := exec.CommandContext(ctx, "notify-send", args...).Output()
		if err != nil {
			slog.Debug("notify-send failed", "title", title, "error", err)
			return
		}
		if strings.TrimSpace(string(out)) != readAction {
			return
		}
		f, err := os.CreateTemp("", "ora-*.md")
		if err != nil {
			return
		}
		f.WriteString(body)
		f.Close()
		if err := exec.Command("zenity", "--text-info", "--title="+title, "--filename="+f.Name(), "--width=720", "--height=560").Run(); err != nil {
			slog.Debug("zenity failed, opening the text with the default app", "error", err)
			exec.Command("xdg-open", f.Name()).Run()
		}
	}()
}

// NotifySend posts a calendar-icon notification; the recorder uses Notify with its own microphone icon.
func NotifySend(title, body string) {
	Notify("x-office-calendar", title, body)
}
