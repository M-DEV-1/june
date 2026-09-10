package proactive

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"ora/internal/memory"
)

// maybeBrief delivers the morning brief once the brief hour has passed and the user's first activity of the day shows up — the newest episode inside activityWindow is that signal. Today's kind='brief' diary row is the once-per-day marker.
func (s *Scheduler) maybeBrief(ctx context.Context) {
	if s.briefHour < 0 {
		return
	}
	now := s.now()
	if now.Hour() < s.briefHour {
		return
	}
	if s.backedOff("brief", now) {
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
		s.failed("brief", now)
		slog.Warn("morning brief failed, backing off", "error", err)
		return
	}
	s.succeeded("brief")
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
	// The brief names the full set: it is the scheduler's own moment, so Done clears it and a snooze re-fires it later. A notice that names none offers only Open (see sendNotice).
	// Open goes to the day's own page, where the brief is written in full (see SetDiaryEntry above and the Days screen); it used to open Tasks with no row, and the card's few lines were the only place the brief could be read (2026-09-09).
	s.say(Notice{Title: "Morning brief", Body: brief, Place: "days", ID: day, Kind: "brief", Actions: noticeActions})
	// ask came from OpenActionItems, which already keeps only the user's own work ("Me" or an unnamed owner) — asking about the first one needs no further ownership check here.
	// The question is answered long after this tick's duty deadline has passed, so the answer is applied on a context that outlives it.
	if len(ask) > 0 {
		go s.askAbout(context.WithoutCancel(ctx), ask[0])
	}
	return nil
}

// The answers the stale-item question offers, in the order they are shown. The keys are what notify-send prints back; the labels are what the user reads.
var askAnswers = []struct{ key, label, status, priority string }{
	{"done", "Done", memory.StatusDone, ""},
	{"dropped", "Not happening", memory.StatusDropped, ""},
	{"low", "Not urgent", "", memory.PriorityLow},
}

// staleNoticeKind is the kind the stale-item question is raised under, on the window's card and in the answer registry alike. It is its own kind because its buttons are its own: a "task" notice offers Done and the three snoozes, this one offers Done, Not happening and Not urgent.
const staleNoticeKind = "stale"

// askAbout puts one stale action item to the user and applies whatever they answer. Dismissing it changes nothing and the item is simply asked about again another morning. Runs in its own goroutine because it blocks until the question is answered, which can be hours.
//
// With a window up the question is a notice of Ora's own carrying its three answers as buttons, and the answer comes back through POST /notices/stale/{note id}/action like every other card button; with no window it is the notify-send banner it has always been. It is deliberately not handed to say, whose fallback posts the five buttons every other notice carries, none of which can answer this question.
//
// ponytail: re-asks every morning until answered. If that grates, stamp the item with the day it was last asked about and leave a gap.
func (s *Scheduler) askAbout(ctx context.Context, a memory.ActionItem) {
	body := fmt.Sprintf("%s — %s\n\nOpen since %s. Any progress?", a.Owner, a.Text, a.Raised.Format(dayFormat))
	n := Notice{Title: "Still open", Body: body, Place: "tasks", ID: strconv.FormatInt(a.NoteID, 10), Kind: staleNoticeKind}
	for _, ans := range askAnswers {
		n.Actions = append(n.Actions, Action{Key: ans.key, Label: ans.label})
	}

	// Applying the answer is handed to Ask rather than done after it returns, so a button pressed once this goroutine has given up still lands on the item: Ask registers it against this notice and Act runs it. Nobody answering changes nothing, and the item comes round again on another morning.
	Ask(n, s.askWait, s.ask, func(chosen string) error { return s.applyStaleAnswer(ctx, a.NoteID, chosen) })
}

// applyStaleAnswer records one answer to the stale-item question against the item it was asked about. Input: the note id and the key of the button pressed. Output: an error when the write failed, which the card reports as the press not having taken; a key none of the answers names changes nothing and is not an error, since the card can only offer these three.
func (s *Scheduler) applyStaleAnswer(ctx context.Context, noteID int64, chosen string) error {
	for _, ans := range askAnswers {
		if ans.key != chosen {
			continue
		}
		if ans.status != "" {
			if err := s.store.SetActionStatus(ctx, noteID, ans.status); err != nil {
				slog.Warn("could not apply the answer to a stale action item", "note_id", noteID, "error", err)
				return err
			}
		}
		if ans.priority != "" {
			if err := s.store.SetActionPriority(ctx, noteID, ans.priority); err != nil {
				slog.Warn("could not apply the answer to a stale action item", "note_id", noteID, "error", err)
				return err
			}
		}
		return nil
	}
	return nil
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
