package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"ora/internal/db"
	"ora/internal/tracker"
)

// defaultPrepTimeout bounds the whole meeting-prep flow, the brain call included. A prep that lands after the meeting has already got going is worse than none — it is noise mid-call rather than a heads-up before it — so a prep not ready within this window is dropped rather than delivered late.
const defaultPrepTimeout = 60 * time.Second

// prepLookback is how far back of screen history identifies the meeting that is about to start: the window title and, if the meeting app shows one, a chat sender's name. Long enough to catch a meeting window that has been open for a few minutes before the user hits record, short enough that it cannot reach back into whatever the user was doing before this call.
const prepLookback = 5 * time.Minute

// noMeetingPrepEnv, set to "1", turns meeting prep off without a rebuild. It is read at the moment prep would fire rather than once at startup, so toggling it takes effect on the very next meeting.
const noMeetingPrepEnv = "ORA_NO_MEETING_PREP"

// prepInstruction tells the brain what to make of a past meeting's minutes when the same meeting is about to happen again. It is principles only — no worked example, no sample sentence to imitate — because a model shown one sentence to copy tends to hand it back with only the names changed.
const prepInstruction = `You are preparing the user for a meeting that is about to start, using the minutes of the last time they met with the same people.

Write two to three sentences of plain spoken text: what was decided last time, what the user still owes from it, and what others owe the user. Say it the way you would say it out loud to someone about to walk into the room, not as a written report.

Do not use markdown, headings, or bullet points, and do not restate the whole transcript or list every attendee. If nothing was decided and nothing is owed either way, say only, briefly, what the meeting was about.`

// prepMeeting composes a one-notification heads-up for the call that is about to start, out of the minutes of the last time the user met these people. Start runs it in its own goroutine, so it never blocks or delays the recording it rides along with, and every failure — a store that cannot answer, nothing on screen naming the meeting, no past meeting to match, a brain call that errors, or one that simply is not back in time — ends the same way: a debug log and nothing sent. A missed prep costs nothing; a wrong or late one costs trust.
func (r *Recorder) prepMeeting() {
	if os.Getenv(noMeetingPrepEnv) == "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.prepTimeout)
	defer cancel()

	now := time.Now()
	episodes, err := r.store.EpisodesInWindow(ctx, now.Add(-prepLookback), now, episodeLimit)
	if err != nil {
		slog.Debug("meeting prep: could not read recent screen context", "error", err)
		return
	}

	title := meetingTitle(episodes)
	participants := meetingParticipants(episodes)
	fragments := meetingTitleFragments(title)
	if len(participants) == 0 && len(fragments) == 0 {
		slog.Debug("meeting prep: nothing on screen names the meeting or its attendees", "title", title)
		return
	}

	note, ok, err := r.lastMatchingMeetingNote(ctx, participants, fragments)
	if err != nil {
		slog.Debug("meeting prep: could not search past meeting minutes", "error", err)
		return
	}
	if !ok {
		slog.Debug("meeting prep: no past minutes match this meeting", "title", title, "participants", participants)
		return
	}

	text, err := r.minutes(ctx, prepPrompt(title, note.Content))
	if err != nil {
		slog.Debug("meeting prep: brain call failed", "error", err)
		return
	}
	// The context can time out during the brain call without the call itself returning an error — a seam under test, or a client that does not check ctx until its next read — so lateness is checked again here rather than trusted to have already turned into an error above.
	if ctx.Err() != nil {
		slog.Debug("meeting prep: ready too late, dropping it")
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		slog.Debug("meeting prep: brain returned nothing")
		return
	}

	head := "Before you join"
	if title != "" {
		head = "Before you join: " + title
	}
	r.notify(head, text)
}

// meetingTitle returns the most recent window title belonging to a call, which is what actually says which meeting this is. It is not simply the last title captured: on 2026-08-31 the user spent a standup in ClickUp and a terminal, so the newest title was "New Tab - Brave" and naming the meeting from it would have been wrong. Falling back to the newest title of any kind is deliberate — a meeting app the pattern does not know is still better named by its window than not at all.
func meetingTitle(eps []db.Episode) string {
	var newest string
	for i := len(eps) - 1; i >= 0; i-- {
		t := strings.TrimSpace(eps[i].Title)
		if t == "" {
			continue
		}
		if isMeetingWindow(eps[i].App, t) {
			return t
		}
		if newest == "" {
			newest = t
		}
	}
	return newest
}

// meetingParticipants pulls candidate names for who is on the call out of recent screen text, using the same pattern primingPrompt mines a chat sender's name from in transcribe.go: a name written immediately before a colon at the start of its own line, which is how a chat window labels who is talking. Unlike primingPrompt, which runs after the meeting to prime whisper, this runs the moment the meeting is detected, against whatever the tracker has already captured, before a single word of transcript exists.
func meetingParticipants(eps []db.Episode) []string {
	return collectMeetingNames(eps, true)
}

// meetingParticipantsInBody is meetingParticipants without the window title as a source. The title names the conversation rather than the people in it, which is a name in a one-to-one call and a meeting's name in a group one, and nothing tells the two apart.
func meetingParticipantsInBody(eps []db.Episode) []string {
	return collectMeetingNames(eps, false)
}

// collectMeetingNames pulls people's names off the meeting's own window. withTitle includes the window title as a source, which is right when the names are only a hint to search past minutes with and wrong when they are counted.
func collectMeetingNames(eps []db.Episode, withTitle bool) []string {
	var names []string
	seen := map[string]bool{}
	for _, e := range eps {
		// Only the call's own window names the people on the call. Any other chat open at the time names people who are not in it.
		if !isMeetingWindow(e.App, e.Title) {
			continue
		}
		sources := []string{e.UserActivity, e.ScreenText, e.VisibleText}
		if withTitle {
			sources = append(sources, e.Title)
		}
		for _, text := range sources {
			add := func(name string) {
				// The app's own name is written on its window as prominently as anybody's: a Teams window reads "Microsoft Teams (PWA) - Chat | Trupti Hosmani | Microsoft Teams", where two of the three capitalised phrases are the software. What names the window cannot also name a person in it.
				if seen[name] || tracker.IsMeetingWindow("", name) || hasChromeWord(name) {
					return
				}
				seen[name] = true
				names = append(names, name)
			}
			for _, m := range chatSenderPattern.FindAllStringSubmatch(text, -1) {
				add(m[1])
			}
			// A meeting window writes the people in the call as plain capitalised names, separated however the app likes — before a colon in a chat log, between pipes in a title bar. Reading them as proper nouns covers every separator without knowing any of them.
			for _, m := range properNounPattern.FindAllString(text, -1) {
				add(m)
			}
		}
	}
	return names
}

// meetingTitleFragments pulls the distinctive words out of a meeting's window title, the same way primingPrompt pulls terms out of screen text: multi-word proper nouns and acronyms first, since those are what actually name a meeting or a client rather than describe the app around it, falling back to individual words long enough to mean something. Every candidate, from either source, is dropped if it is app furniture ("Google Chrome", "Zoom Meeting") rather than something the meeting is about — the same hasChromeWord check primingPrompt runs every term through.
func meetingTitleFragments(title string) []string {
	var frags []string
	add := func(cands []string) {
		for _, c := range cands {
			if !hasChromeWord(c) {
				frags = append(frags, c)
			}
		}
	}
	add(properNounPattern.FindAllString(title, -1))
	add(acronymPattern.FindAllString(title, -1))
	if len(frags) > 0 {
		return frags
	}
	for _, w := range strings.Fields(title) {
		if len(w) >= 4 && !hasChromeWord(w) {
			frags = append(frags, w)
		}
	}
	return frags
}

// meetingNoteMatches reports whether a past meeting's minutes are about the same meeting as the one on screen now: either a participant named on screen is also named in those minutes, or a distinctive word from the current meeting's title shows up in them. Both comparisons are case-insensitive, since a chat sender's name and the same name typed into minutes rarely share capitalisation exactly.
func meetingNoteMatches(content string, participants, titleFragments []string) bool {
	lc := strings.ToLower(content)
	for _, p := range participants {
		if strings.Contains(lc, strings.ToLower(p)) {
			return true
		}
	}
	for _, f := range titleFragments {
		if strings.Contains(lc, strings.ToLower(f)) {
			return true
		}
	}
	return false
}

// lastMatchingMeetingNote scans the store's meeting notes for the most recent one that matches this meeting, by participant or by title. GetNotes already orders every note newest first, so the first meeting-kind match found is the most recent one — there are few enough meeting notes that a plain scan needs nothing cleverer.
func (r *Recorder) lastMatchingMeetingNote(ctx context.Context, participants, titleFragments []string) (db.Note, bool, error) {
	notes, err := r.store.GetNotes(ctx)
	if err != nil {
		return db.Note{}, false, err
	}
	for _, n := range notes {
		if n.Kind != noteKind {
			continue
		}
		if meetingNoteMatches(n.Content, participants, titleFragments) {
			return n, true, nil
		}
	}
	return db.Note{}, false, nil
}

// prepPrompt assembles the brain input for a prep note: the instruction, which meeting is about to start, and the minutes of the last time it happened.
func prepPrompt(title, minutes string) string {
	var b strings.Builder
	b.WriteString(prepInstruction)
	if title != "" {
		fmt.Fprintf(&b, "\n\nThe meeting about to start: %s\n", title)
	}
	b.WriteString("\nMinutes from the last time they met:\n")
	b.WriteString(minutes)
	return b.String()
}
