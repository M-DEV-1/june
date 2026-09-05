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
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Priya Shah: ok sure ping me"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nPriya Shah agreed to send the deck by Friday.", noteKind); err != nil {
		t.Fatal(err)
	}
	var got notifications
	r.notify = got.add
	var gotPrompt string
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		gotPrompt = prompt
		return "Priya still owes the deck from last time.", nil
	}

	r.prepMeeting()

	if !got.contains("deck") {
		t.Errorf("expected a prep notification mentioning the deck, got %v", got.sent)
	}
	if !strings.Contains(gotPrompt, "Priya Shah agreed to send the deck") {
		t.Errorf("prompt is missing the matched minutes:\n%s", gotPrompt)
	}
	if strings.Contains(prepInstruction, "For example") {
		t.Error("prepInstruction must be principles only, not a worked example")
	}
}

// A shared title fragment ("Acme Corp") is enough to match even when no chat sender was on screen. The window is the call's own — a title that is not a meeting window names nothing, whatever words are in it.
func TestPrepMeeting_MatchesByTitleFragment(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet – Weekly sync with Acme Corp"}}}
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
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Priya Shah: hello"}}}
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
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Priya Shah: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nPriya Shah: older meeting, decided the budget.", noteKind); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nPriya Shah: newest meeting, decided the venue.", noteKind); err != nil {
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
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Priya Shah: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nPriya Shah agreed to send the deck.", noteKind); err != nil {
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
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Priya Shah: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nPriya Shah agreed to send the deck.", noteKind); err != nil {
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
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Priya Shah: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nPriya Shah agreed to send the deck.", noteKind); err != nil {
		t.Fatal(err)
	}
	var got notifications
	r.notify = got.add
	r.minutes = func(ctx context.Context, prompt string) (string, error) { return "Priya still owes the deck.", nil }

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

// A Google Meet window is titled after a room code and the browser's own indicators — "Meet – abc-defg-hij - Microphone recording - Brave" — so every word in it except the code comes from the browser. Matching on any one of them picked an unrelated meeting from a week earlier, because "meet" and "recording" appear in all twelve past notes and identify nothing. The room code identifies the room and matches no minutes, so the right answer is to say nothing.
func TestPickMeetingNote_DeclinesWhenTheTitleIsOnlyBrowserChrome(t *testing.T) {
	frags := meetingTitleFragments("Meet \u2013 abc-defg-hij - Microphone recording - Brave")

	if _, ok := pickMeetingNote(twelveMeetingNotes(), nil, frags, pastTitles()); ok {
		t.Error("matched a past meeting on words that appear in every past meeting")
	}
}

// A name read off the meeting window is the strong signal and still matches on its own.
func TestPickMeetingNote_MatchesOnAParticipant(t *testing.T) {
	notes := append(twelveMeetingNotes(), db.Note{ID: 99, Kind: "meeting", Content: "# Sync\nPriya walked through the value chain work."})

	got, ok := pickMeetingNote(notes, []string{"Priya"}, nil, pastTitles())

	if !ok || got.ID != 99 {
		t.Errorf("pickMeetingNote = %d,%v, want the note naming the participant", got.ID, ok)
	}
}

// collectMeetingNames must read a real call window's roster without pulling in the interface around it: mute buttons, presence labels, "(Host)"/"(You)"/"(Presenting)" tags, and the app's own name. Fixtures are the flattened single-string shape an accessibility tree actually produces for a participants pane — one long run of text, not one name per line.
func TestCollectMeetingNames_RostersFromRealApps(t *testing.T) {
	cases := []struct {
		name  string
		title string
		text  string
		want  []string
		deny  []string
	}{
		{
			// Accessibility trees read one node's text per line; a participants pane is one name (or one button) per line, same as the chat log chatSenderPattern already assumes.
			name:  "google meet",
			title: "Meet - team-sync-call - Brave",
			text:  "People (3)\nSam Iyer (Host)\nPriya Shah (Presenting)\nRohit Verma\nYou\nMute\nCamera\nPresent now\nMore options\nLeave call",
			want:  []string{"Sam Iyer", "Priya Shah", "Rohit Verma"},
			deny:  []string{"Mute", "Camera", "Present", "More", "Leave", "You", "Sam Iyer (Host)", "Priya Shah (Presenting)"},
		},
		{
			name:  "teams",
			title: "Microsoft Teams (PWA) - Chat | Priya Shah | Microsoft Teams",
			text:  "Participants (2)\nPriya Shah (Organizer)\nVikram Goel\nYou\nRaise Hand\nReact\nMore\nLeave",
			want:  []string{"Priya Shah", "Vikram Goel"},
			deny:  []string{"Microsoft Teams", "Raise Hand", "Chat", "More", "Leave", "You", "Priya Shah (Organizer)"},
		},
		{
			name:  "zoom",
			title: "Zoom Meeting",
			text:  "Participants (2)\nKaran Mehta (Host, me)\nNeha Kapoor\nMute All\nUnmute\nStop Video\nShare Screen\nRecord\nEnd Meeting",
			want:  []string{"Karan Mehta", "Neha Kapoor"},
			deny:  []string{"Zoom Meeting", "Share Screen", "Mute All", "Stop Video", "End Meeting", "Karan Mehta (Host, me)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectMeetingNames([]db.Episode{{Title: tc.title, ScreenText: tc.text}}, true, nil)
			for _, w := range tc.want {
				if !contains(got, w) {
					t.Errorf("%s: got %v, want it to include %q", tc.name, got, w)
				}
			}
			for _, d := range tc.deny {
				if contains(got, d) {
					t.Errorf("%s: got %v, must not include interface text %q", tc.name, got, d)
				}
			}
		})
	}
}

// The same person named twice — once tagged with a role in the roster, once as a chat sender with no tag — must collapse to one attendee, not two.
func TestCollectMeetingNames_CollapsesDuplicatesAcrossRoleTags(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - team-sync - Brave", ScreenText: "Sam Iyer (Host) Priya Shah"},
		{Title: "Meet - team-sync - Brave", ScreenText: "Sam Iyer: let's get started"},
	}

	got := collectMeetingNames(eps, true, nil)

	count := 0
	for _, n := range got {
		if n == "Sam Iyer" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("collectMeetingNames = %v, want exactly one \"Sam Iyer\", got %d", got, count)
	}
}

// A name already known from personal context is kept even when its shape would otherwise get it dropped — here a five-word name past the four-word limit ordinary candidates are held to.
func TestCollectMeetingNames_KeepsAKnownNameEvenWhenUnusual(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - family catch-up - Brave", ScreenText: "Sri Lakshmi Venkata Subramaniam Iyer\nYou\nMute\nLeave call"},
	}
	known := personNamesFromContext([]db.PersonalEntry{{Subject: "sri-lakshmi-venkata-subramaniam-iyer", Content: "the user's aunt"}})

	got := collectMeetingNames(eps, true, known)

	if !contains(got, "Sri Lakshmi Venkata Subramaniam Iyer") {
		t.Errorf("collectMeetingNames = %v, want the known long name kept", got)
	}
}

// Two capitalised interface words strung together must never come out as a name, known or not — an app's toolbar is never a person, however name-shaped the phrase reads.
func TestCollectMeetingNames_KnownInterfaceWordsNeverBecomeAName(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - standup - Brave", ScreenText: "Raise Hand\nMore Options\nShare Screen"},
	}

	got := collectMeetingNames(eps, true, nil)

	if len(got) != 0 {
		t.Errorf("collectMeetingNames = %v, want no names from pure interface text", got)
	}
}

// A name written in a script without case — Devanagari here, Tamil there — must reach the participant list the same way a Latin name does, each on its own line the way a participants pane lists one name per line, with the usual interface furniture around it still dropped.
func TestCollectMeetingNames_ScriptsWithoutCaseReachParticipants(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - team-sync - Brave", ScreenText: "People (2)\nराहुल\nYou\nMute\nLeave call"},
		{Title: "Meet - team-sync - Brave", ScreenText: "Participants (2)\nபிரியா\nYou\nMute\nLeave call"},
	}

	got := collectMeetingNames(eps, true, nil)

	for _, want := range []string{"राहुल", "பிரியா"} {
		if !contains(got, want) {
			t.Errorf("collectMeetingNames = %v, want it to include %q", got, want)
		}
	}
	for _, deny := range []string{"Mute", "Leave", "You"} {
		if contains(got, deny) {
			t.Errorf("collectMeetingNames = %v, must not include interface text %q", got, deny)
		}
	}
}

// A Hinglish meeting mixes a Latin name and a Devanagari name on the same line, and both must be read off it — the Latin one by the existing capitalised-word rule, the Devanagari one by the caseless-script pass added beside it.
func TestCollectMeetingNames_MixedHinglishLineYieldsBothScripts(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - team-sync - Brave", ScreenText: "Rohit Verma राहुल\nMute\nLeave call"},
	}

	got := collectMeetingNames(eps, true, nil)

	if !contains(got, "Rohit Verma") {
		t.Errorf("collectMeetingNames = %v, want the Latin name %q", got, "Rohit Verma")
	}
	if !contains(got, "राहुल") {
		t.Errorf("collectMeetingNames = %v, want the Devanagari name %q", got, "राहुल")
	}
}

// Cyrillic has an upper and lower form, so it belongs to the capitalised-word rule rather than the caseless-script pass — this pins the boundary the new rule draws, so a cased script never gets swept in beside Devanagari or Tamil.
func TestCaselessScriptNames_ScriptsWithCaseAreLeftToTheCapitalisedRule(t *testing.T) {
	got := caselessScriptNames("Иван Петров")
	if len(got) != 0 {
		t.Errorf("caselessScriptNames(%q) = %v, want none: Cyrillic has case", "Иван Петров", got)
	}
}

// A name written in a script without letter case has no "all caps" form, so the shouting check must leave it alone, while a Latin word in all caps is still interface furniture. The proper-noun matcher that feeds collectMeetingNames is Latin-only, so this checks the shape rule on its own.
func TestLooksLikeName_ScriptsWithoutCaseAreNotShouting(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"प्रिया नायर", true},
		{"田中 太郎", true},
		{"Sam Iyer", true},
		{"MUTE", false},
		{"Share Screen 2", false},
	}
	for _, c := range cases {
		if got := looksLikeName(c.name); got != c.want {
			t.Errorf("looksLikeName(%q) = %v, want %v", c.name, got, c.want)
		}
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

// A Chinese meeting window writes its buttons, its chat and a person's name in the same script, and nothing in that script is capitalised, so the caseless pass has only the line's own shape to go on. A line of chat text is not a roster line, and nothing on it — least of all the app's own words for mute, leave and participants — is a person on the call.
func TestCollectMeetingNames_CJKChatTextIsNotAParticipant(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - team-sync - Brave", ScreenText: "李伟: 静音 离开会议 参会者 我马上加入会议\n王芳: 好的 我们开始吧 请大家静音\nMute\nLeave call"},
	}

	got := collectMeetingNames(eps, true, nil)

	if len(got) != 0 {
		t.Errorf("collectMeetingNames = %v, want no names from a window whose chat is written in a caseless script", got)
	}
}

// A roster lists one name per line, and a name in a caseless script is written across two words there just as a Latin one is. It must arrive as the whole name rather than as its two words separately, since half a name matches nothing in past minutes.
func TestCollectMeetingNames_CaselessRosterLineIsOneWholeName(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - team-sync - Brave", ScreenText: "People (3)\nराहुल शर्मा\nRohit Verma\nYou\nMute\nLeave call"},
	}

	got := collectMeetingNames(eps, true, nil)

	if !contains(got, "राहुल शर्मा") {
		t.Errorf("collectMeetingNames = %v, want the whole two-word name %q", got, "राहुल शर्मा")
	}
	if contains(got, "राहुल") || contains(got, "शर्मा") {
		t.Errorf("collectMeetingNames = %v, want no half of the name on its own", got)
	}
}

// The same Chinese meeting window without a single colon on it: a line of chat text, a toolbar button and a pane heading, each alone on its own line. Han is written without spaces between words, so every one of those lines is one unbroken run of letters exactly as a Chinese name would be, and nothing in the shape of "我马上加入会议" ("I'll join the meeting shortly") tells it from a person. None of the three is a participant.
func TestCollectMeetingNames_CJKScreenTextWithoutAColonIsNotAParticipant(t *testing.T) {
	eps := []db.Episode{
		{Title: "Meet - team-sync - Brave", ScreenText: "我马上加入会议\n静音\n参会者"},
	}

	got := collectMeetingNames(eps, true, nil)

	if len(got) != 0 {
		t.Errorf("collectMeetingNames = %v, want no names from unspaced screen text with no colon on it", got)
	}
}

// A call in an app the meeting-window pattern does not list leaves no meeting window on screen at all, and the last window that was there names something else entirely. Naming the meeting after it — and then searching memory for its words — produced "Before you join: Gmail — Inbox (12)". Nothing on screen names the meeting, so prep stays silent.
func TestPrepMeeting_SilentWhenNoWindowOnScreenIsACall(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{App: "Brave", Title: "Inbox (12) - Acme Corp - Brave"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nAcme Corp asked for the Q3 numbers.", noteKind); err != nil {
		t.Fatal(err)
	}
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		t.Fatal("the brain must not be called when nothing on screen is a call")
		return "", nil
	}
	var got notifications
	r.notify = got.add

	r.prepMeeting()

	if len(got.sent) != 0 {
		t.Errorf("expected silence when no window on screen is a call, got %v", got.sent)
	}
}
