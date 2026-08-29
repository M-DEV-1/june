package recorder

import (
	"context"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// meetingParticipants finds a name a chat window wrote before a colon, the same shape primingPrompt mines for whisper — proving prep reuses that mechanism rather than reinventing it.
func TestMeetingParticipants_ExtractsChatSenderNames(t *testing.T) {
	eps := []db.Episode{{ScreenText: "Trupti Hosmani: ok sure ping me | participating in a video call"}}
	got := meetingParticipants(eps)
	if len(got) != 1 || got[0] != "Trupti Hosmani" {
		t.Errorf("meetingParticipants = %v, want [Trupti Hosmani]", got)
	}
}

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
	store := &fakeStore{episodes: []db.Episode{{ScreenText: "Trupti Hosmani: ok sure ping me"}}}
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
	r.minutes = func(ctx context.Context, prompt string) (string, error) { return "Acme Corp is still waiting on the Q3 numbers.", nil }

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
	store := &fakeStore{episodes: []db.Episode{{ScreenText: "Trupti Hosmani: hello"}}}
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
	store := &fakeStore{episodes: []db.Episode{{ScreenText: "Trupti Hosmani: hello"}}}
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
	store := &fakeStore{episodes: []db.Episode{{ScreenText: "Trupti Hosmani: hello"}}}
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
	store := &fakeStore{episodes: []db.Episode{{ScreenText: "Trupti Hosmani: hello"}}}
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
	store := &fakeStore{episodes: []db.Episode{{ScreenText: "Trupti Hosmani: hello"}}}
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
