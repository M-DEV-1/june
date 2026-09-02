package recorder

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// A window title names a meeting through its proper nouns, not through app furniture around them.
func TestMeetingTitleFragments_PrefersProperNounsOverChrome(t *testing.T) {
	got := meetingTitleFragments("Meet – Acme Corp weekly sync - Google Chrome")
	if !contains(got, "Acme Corp") {
		t.Errorf("meetingTitleFragments(%q) = %v, want it to include %q", "Meet – Acme Corp weekly sync - Google Chrome", got, "Acme Corp")
	}
	for _, f := range got {
		if strings.EqualFold(f, "Google Chrome") || strings.EqualFold(f, "Chrome") {
			t.Errorf("meetingTitleFragments picked up app chrome: %v", got)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// A participant on screen now who is also named in a past meeting's minutes is what identifies that meeting as the same one.
func TestPrepMeeting_MatchesByParticipantAndNotifies(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Trupti Hosmani: ok sure ping me"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nTrupti Hosmani agreed to send the deck by Friday.", noteKind); err != nil {
		t.Fatal(err)
	}
	var got notifications
	r.notify = got.add
	var gotPrompt string
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		gotPrompt = prompt
		return "Trupti still owes the deck from last time.", nil
	}

	r.prepMeeting()

	if !got.contains("deck") {
		t.Errorf("expected a prep notification mentioning the deck, got %v", got.sent)
	}
	if !strings.Contains(gotPrompt, "Trupti Hosmani agreed to send the deck") {
		t.Errorf("prompt is missing the matched minutes:\n%s", gotPrompt)
	}
	if strings.Contains(prepInstruction, "For example") {
		t.Error("prepInstruction must be principles only, not a worked example")
	}
}

// A shared title fragment ("Acme Corp") is enough to match even when no chat sender was on screen.
func TestPrepMeeting_MatchesByTitleFragment(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Weekly sync - Acme Corp"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nAcme Corp asked for the Q3 numbers.", noteKind); err != nil {
		t.Fatal(err)
	}
	var got notifications
	r.notify = got.add
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		return "Acme Corp is still waiting on the Q3 numbers.", nil
	}

	r.prepMeeting()

	if !got.contains("q3") {
		t.Errorf("expected a prep notification about the Q3 numbers, got %v", got.sent)
	}
}

// Silence is correct when nothing on screen names either the meeting or its attendees — there's nothing to search memory for.
func TestPrepMeeting_SilentWhenNothingOnScreenNamesTheMeeting(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "call", ScreenText: "just a plain video call"}}}
	r, _, _ := newTestRecorder(t, store)
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		t.Fatal("the brain must not be called when nothing identifies the meeting")
		return "", nil
	}
	var got notifications
	r.notify = got.add

	r.prepMeeting()

	if len(got.sent) != 0 {
		t.Errorf("expected silence, got %v", got.sent)
	}
}

// Silence is also correct when the meeting is identified but memory has nothing from a prior instance of it.
func TestPrepMeeting_SilentWhenNoPastMinutesMatch(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Trupti Hosmani: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	var got notifications
	r.notify = got.add

	r.prepMeeting()

	if len(got.sent) != 0 {
		t.Errorf("expected silence with no prior minutes filed, got %v", got.sent)
	}
}

// The most recently filed matching note wins, not just any match, since GetNotes already orders newest first.
func TestPrepMeeting_MostRecentMatchWins(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Trupti Hosmani: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nTrupti Hosmani: older meeting, decided the budget.", noteKind); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nTrupti Hosmani: newest meeting, decided the venue.", noteKind); err != nil {
		t.Fatal(err)
	}
	var gotPrompt string
	r.minutes = func(ctx context.Context, prompt string) (string, error) { gotPrompt = prompt; return "ok", nil }

	r.prepMeeting()

	if !strings.Contains(gotPrompt, "newest meeting") || strings.Contains(gotPrompt, "older meeting") {
		t.Errorf("expected the most recent matching note in the prompt, got:\n%s", gotPrompt)
	}
}

// ORA_NO_MEETING_PREP=1 is the alpha kill switch: it must stop prep even when everything else would fire it.
func TestPrepMeeting_KillSwitchDisablesIt(t *testing.T) {
	t.Setenv(noMeetingPrepEnv, "1")
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Trupti Hosmani: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nTrupti Hosmani agreed to send the deck.", noteKind); err != nil {
		t.Fatal(err)
	}
	var got notifications
	r.notify = got.add
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		t.Fatal("the brain must not be called with the kill switch on")
		return "", nil
	}

	r.prepMeeting()

	if len(got.sent) != 0 {
		t.Errorf("expected silence with the kill switch on, got %v", got.sent)
	}
}

// A brain call that is not back within the timeout must be dropped, even if it eventually returns text — a prep that lands mid-meeting is noise, not help.
func TestPrepMeeting_DropsSilentlyWhenTheBrainIsTooSlow(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Trupti Hosmani: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nTrupti Hosmani agreed to send the deck.", noteKind); err != nil {
		t.Fatal(err)
	}
	r.prepTimeout = 10 * time.Millisecond
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		time.Sleep(50 * time.Millisecond) // ignores ctx on purpose, to prove prep checks lateness itself
		return "too late to matter", nil
	}
	var got notifications
	r.notify = got.add

	r.prepMeeting()

	if len(got.sent) != 0 {
		t.Errorf("expected the late prep to be dropped, got %v", got.sent)
	}
}

// Start is the call-detection hook: it must fire prep without making the caller wait for it.
func TestStart_FiresMeetingPrepAsynchronously(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Trupti Hosmani: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nTrupti Hosmani agreed to send the deck.", noteKind); err != nil {
		t.Fatal(err)
	}
	var got notifications
	r.notify = got.add
	r.minutes = func(ctx context.Context, prompt string) (string, error) { return "Trupti still owes the deck.", nil }

	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for !got.contains("deck") {
		select {
		case <-deadline:
			t.Fatal("meeting prep never notified")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// twelveMeetingNotes mirrors the real store: every meeting note contains the words "meet" and "recording", because minutes are about meetings and say so, and two of them happen to mention a microphone. Measured on this machine's 12 meeting notes on 2026-09-01.
func twelveMeetingNotes() []db.Note {
	notes := make([]db.Note, 0, 12)
	for i := 0; i < 12; i++ {
		c := fmt.Sprintf("# Meeting %d\nWe met and this is the recording of what was decided.", i)
		if i == 7 || i == 9 {
			c += " Someone's microphone was cutting out."
		}
		notes = append(notes, db.Note{ID: int64(i), Kind: "meeting", Content: c})
	}
	return notes
}

// pastTitles is the shape of this machine's real history: the browser's own words recur across every title, and a meeting's own words appear once or twice.
func pastTitles() []string {
	var ts []string
	for i := 0; i < 20; i++ {
		ts = append(ts, fmt.Sprintf("Meet - room-%d - Microphone recording - Brave", i))
	}
	return append(ts, "Calendar | Zitadel auth review | Microsoft Teams - Brave")
}

// A Google Meet window is titled after a room code and the browser's own indicators — "Meet – xha-yzim-osg - Microphone recording - Brave" — so every word in it except the code comes from the browser. Matching on any one of them picked an unrelated meeting from a week earlier, because "meet" and "recording" appear in all twelve past notes and identify nothing. The room code identifies the room and matches no minutes, so the right answer is to say nothing.
func TestPickMeetingNote_DeclinesWhenTheTitleIsOnlyBrowserChrome(t *testing.T) {
	frags := meetingTitleFragments("Meet \u2013 xha-yzim-osg - Microphone recording - Brave")

	if _, ok := pickMeetingNote(twelveMeetingNotes(), nil, frags, pastTitles()); ok {
		t.Error("matched a past meeting on words that appear in every past meeting")
	}
}

// A name read off the meeting window is the strong signal and still matches on its own.
func TestPickMeetingNote_MatchesOnAParticipant(t *testing.T) {
	notes := append(twelveMeetingNotes(), db.Note{ID: 99, Kind: "meeting", Content: "# Sync\nTrupti walked through the value chain work."})

	got, ok := pickMeetingNote(notes, []string{"Trupti"}, nil, pastTitles())

	if !ok || got.ID != 99 {
		t.Errorf("pickMeetingNote = %d,%v, want the note naming the participant", got.ID, ok)
	}
}

// The word that only ever appears in one title is the one that names the meeting, and it matches on its own. The common word beside it is ignored rather than allowed to match everything.
func TestPickMeetingNote_MatchesOnTheRareWordAndIgnoresTheCommonOne(t *testing.T) {
	notes := append(twelveMeetingNotes(),
		db.Note{ID: 42, Kind: "meeting", Content: "# Zitadel auth\nWe agreed the Zitadel migration."},
	)

	got, ok := pickMeetingNote(notes, nil, []string{"Meet", "Zitadel"}, pastTitles())

	if !ok || got.ID != 42 {
		t.Errorf("pickMeetingNote = %d,%v, want the note sharing the rare word", got.ID, ok)
	}
}
