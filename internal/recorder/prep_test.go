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

// Every case below is silence: nothing on screen names the meeting, the meeting is named but memory has nothing from a prior instance, the alpha kill switch is on, or nothing on screen is even a call window. Whatever the reason, the brain must never be called and nothing must be sent.
func TestPrepMeeting_Silent(t *testing.T) {
	cases := []struct {
		name        string
		episodes    []db.Episode
		noteContent string // logged first when non-empty
		killSwitch  bool
	}{
		{
			name:     "nothing on screen names the meeting",
			episodes: []db.Episode{{Title: "call", ScreenText: "just a plain video call"}},
		},
		{
			name:     "the meeting is identified but no past minutes match",
			episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Vexil Quorin: hello"}},
		},
		{
			name:        "ORA_NO_MEETING_PREP=1 is the alpha kill switch",
			episodes:    []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Vexil Quorin: hello"}},
			noteContent: "# Meeting minutes\n\nVexil Quorin agreed to send the deck.",
			killSwitch:  true,
		},
		{
			// A call in an app the meeting-window pattern does not list leaves no meeting window on screen at all, and the last window that was there names something else entirely. Naming the meeting after it — and then searching memory for its words — produced "Before you join: Gmail — Inbox (12)".
			name:        "nothing on screen is a call window",
			episodes:    []db.Episode{{App: "Brave", Title: "Inbox (12) - Acme Corp - Brave"}},
			noteContent: "# Meeting minutes\n\nAcme Corp asked for the Q3 numbers.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.killSwitch {
				t.Setenv(noMeetingPrepEnv, "1")
			}
			store := &fakeStore{episodes: c.episodes}
			r, _, _ := newTestRecorder(t, store)
			if c.noteContent != "" {
				if _, err := store.LogNote(context.Background(), c.noteContent, noteKind); err != nil {
					t.Fatal(err)
				}
			}
			r.minutes = func(ctx context.Context, prompt string) (string, error) {
				t.Fatal("the brain must not be called")
				return "", nil
			}
			var got notifications
			r.notifyAt = got.addAt

			r.prepMeeting()

			if len(got.sent) != 0 {
				t.Errorf("expected silence, got %v", got.sent)
			}
		})
	}
}

// A participant on screen who is also named in a past meeting's minutes, or a shared title fragment on its own, or several matching notes with the newest one winning: each of these identifies the right past meeting and notifies about it.
func TestPrepMeeting_Matches(t *testing.T) {
	if strings.Contains(prepInstruction, "For example") {
		t.Error("prepInstruction must be principles only, not a worked example")
	}

	cases := []struct {
		name        string
		episodes    []db.Episode
		notes       []string // logged in order filed
		reply       string
		wantContain string
		checkPrompt func(t *testing.T, prompt string)
	}{
		{
			name:        "matches by participant and notifies",
			episodes:    []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Vexil Quorin: ok sure ping me"}},
			notes:       []string{"# Meeting minutes\n\nVexil Quorin agreed to send the deck by Friday."},
			reply:       "Vexil still owes the deck from last time.",
			wantContain: "deck",
			checkPrompt: func(t *testing.T, prompt string) {
				if !strings.Contains(prompt, "Vexil Quorin agreed to send the deck") {
					t.Errorf("prompt is missing the matched minutes:\n%s", prompt)
				}
			},
		},
		{
			// A shared title fragment ("Acme Corp") is enough to match even when no chat sender was on screen.
			name:        "matches by title fragment alone",
			episodes:    []db.Episode{{Title: "Meet – Weekly sync with Acme Corp"}},
			notes:       []string{"# Meeting minutes\n\nAcme Corp asked for the Q3 numbers."},
			reply:       "Acme Corp is still waiting on the Q3 numbers.",
			wantContain: "q3",
		},
		{
			// The most recently filed matching note wins, not just any match, since GetNotes already orders newest first.
			name:     "the most recent matching note wins",
			episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Vexil Quorin: hello"}},
			notes: []string{
				"# Meeting minutes\n\nVexil Quorin: older meeting, decided the budget.",
				"# Meeting minutes\n\nVexil Quorin: newest meeting, decided the venue.",
			},
			reply: "ok",
			checkPrompt: func(t *testing.T, prompt string) {
				if !strings.Contains(prompt, "newest meeting") || strings.Contains(prompt, "older meeting") {
					t.Errorf("expected the most recent matching note in the prompt, got:\n%s", prompt)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{episodes: c.episodes}
			r, _, _ := newTestRecorder(t, store)
			for _, n := range c.notes {
				if _, err := store.LogNote(context.Background(), n, noteKind); err != nil {
					t.Fatal(err)
				}
			}
			var got notifications
			r.notifyAt = got.addAt
			var gotPrompt string
			r.minutes = func(ctx context.Context, prompt string) (string, error) {
				gotPrompt = prompt
				return c.reply, nil
			}

			r.prepMeeting()

			if c.wantContain != "" && !got.contains(c.wantContain) {
				t.Errorf("expected a prep notification containing %q, got %v", c.wantContain, got.sent)
			}
			if c.checkPrompt != nil {
				c.checkPrompt(t, gotPrompt)
			}
		})
	}
}

// A brain call that is not back within the timeout must be dropped, even if it eventually returns text — a prep that lands mid-meeting is noise, not help. This needs its own deadline configuration and a real sleep, so it cannot share the table above.
func TestPrepMeeting_DropsSilentlyWhenTheBrainIsTooSlow(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Vexil Quorin: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nVexil Quorin agreed to send the deck.", noteKind); err != nil {
		t.Fatal(err)
	}
	r.prepTimeout = 10 * time.Millisecond
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		time.Sleep(50 * time.Millisecond) // ignores ctx on purpose, to prove prep checks lateness itself
		return "too late to matter", nil
	}
	var got notifications
	r.notifyAt = got.addAt

	r.prepMeeting()

	if len(got.sent) != 0 {
		t.Errorf("expected the late prep to be dropped, got %v", got.sent)
	}
}

// Start is the call-detection hook: it must fire prep without making the caller wait for it. This polls for an async result, so it cannot share the table above.
func TestStart_FiresMeetingPrepAsynchronously(t *testing.T) {
	store := &fakeStore{episodes: []db.Episode{{Title: "Meet - abc-defg-hij - Brave", ScreenText: "Vexil Quorin: hello"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nVexil Quorin agreed to send the deck.", noteKind); err != nil {
		t.Fatal(err)
	}
	var got notifications
	r.notifyAt = got.addAt
	r.minutes = func(ctx context.Context, prompt string) (string, error) { return "Vexil still owes the deck.", nil }

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

// pickMeetingNote either declines on words every past meeting shares, or matches on a participant named on screen, or on the one word that is rare enough across past titles to identify a meeting instead of matching everything.
func TestPickMeetingNote(t *testing.T) {
	cases := []struct {
		name         string
		notes        []db.Note
		participants []string
		fragments    []string
		wantOK       bool
		wantID       int64
	}{
		{
			// A Google Meet window is titled after a room code and the browser's own indicators, so every word in it except the code comes from the browser. The room code identifies the room and matches no minutes, so the right answer is to say nothing.
			name:      "declines when the title is only browser chrome",
			notes:     twelveMeetingNotes(),
			fragments: meetingTitleFragments("Meet – abc-defg-hij - Microphone recording - Brave"),
			wantOK:    false,
		},
		{
			name:         "matches on a participant named on screen",
			notes:        append(twelveMeetingNotes(), db.Note{ID: 99, Kind: "meeting", Content: "# Sync\nVexil walked through the value chain work."}),
			participants: []string{"Vexil"},
			wantOK:       true,
			wantID:       99,
		},
		{
			// The word that only ever appears in one title is the one that names the meeting, and it matches on its own. The common word beside it is ignored rather than allowed to match everything.
			name:      "matches on the rare word and ignores the common one",
			notes:     append(twelveMeetingNotes(), db.Note{ID: 42, Kind: "meeting", Content: "# Zitadel auth\nWe agreed the Zitadel migration."}),
			fragments: []string{"Meet", "Zitadel"},
			wantOK:    true,
			wantID:    42,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := pickMeetingNote(c.notes, c.participants, c.fragments, pastTitles())
			if ok != c.wantOK {
				t.Fatalf("pickMeetingNote ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got.ID != c.wantID {
				t.Errorf("pickMeetingNote = %d, want %d", got.ID, c.wantID)
			}
		})
	}
}

// collectMeetingNames must read a real call window's roster without pulling in the interface around it — mute buttons, presence labels, role tags, the app's own name — must collapse the same person tagged twice into one attendee, must keep a name already known from personal context even when its shape is unusual, and must read a script with no upper or lower form (Devanagari, Tamil, CJK) by its own shape rule beside the capitalised-word rule that reads Latin and Hinglish names.
func TestCollectMeetingNames(t *testing.T) {
	cases := []struct {
		name     string
		eps      []db.Episode
		known    []string
		want     []string // each must be present
		wantOnce []string // each must be present exactly once
		deny     []string // each must be absent
		wantNone bool     // got must be empty
	}{
		{
			// Fixtures are the flattened single-string shape an accessibility tree actually produces for a participants pane — one long run of text, not one name per line.
			name: "google meet roster ignores mute/host tags and the app's own name",
			eps:  []db.Episode{{Title: "Meet - team-sync-call - Brave", ScreenText: "People (3)\nEmzor Wandel (Host)\nVexil Quorin (Presenting)\nYalven Pravik\nYou\nMute\nCamera\nPresent now\nMore options\nLeave call"}},
			want: []string{"Emzor Wandel", "Vexil Quorin", "Yalven Pravik"},
			deny: []string{"Mute", "Camera", "Present", "More", "Leave", "You", "Emzor Wandel (Host)", "Vexil Quorin (Presenting)"},
		},
		{
			name: "teams roster ignores its own app name and toolbar",
			eps:  []db.Episode{{Title: "Microsoft Teams (PWA) - Chat | Vexil Quorin | Microsoft Teams", ScreenText: "Participants (2)\nVexil Quorin (Organizer)\nRavix Dolmen\nYou\nRaise Hand\nReact\nMore\nLeave"}},
			want: []string{"Vexil Quorin", "Ravix Dolmen"},
			deny: []string{"Microsoft Teams", "Raise Hand", "Chat", "More", "Leave", "You", "Vexil Quorin (Organizer)"},
		},
		{
			name: "zoom roster ignores its own controls",
			eps:  []db.Episode{{Title: "Zoom Meeting", ScreenText: "Participants (2)\nTrelvo Kordis (Host, me)\nFenrik Halvo\nMute All\nUnmute\nStop Video\nShare Screen\nRecord\nEnd Meeting"}},
			want: []string{"Trelvo Kordis", "Fenrik Halvo"},
			deny: []string{"Zoom Meeting", "Share Screen", "Mute All", "Stop Video", "End Meeting", "Trelvo Kordis (Host, me)"},
		},
		{
			// The same person named twice — once tagged with a role in the roster, once as a chat sender with no tag — must collapse to one attendee, not two.
			name: "the same person tagged twice collapses to one attendee",
			eps: []db.Episode{
				{Title: "Meet - team-sync - Brave", ScreenText: "Emzor Wandel (Host) Vexil Quorin"},
				{Title: "Meet - team-sync - Brave", ScreenText: "Emzor Wandel: let's get started"},
			},
			wantOnce: []string{"Emzor Wandel"},
		},
		{
			// A name already known from personal context is kept even when its shape would otherwise get it dropped — here a five-word name past the four-word limit ordinary candidates are held to.
			name:  "a known long name is kept even past the ordinary word-count limit",
			eps:   []db.Episode{{Title: "Meet - family catch-up - Brave", ScreenText: "Ovren Kelvara Tumbrel Emzoran Wandel\nYou\nMute\nLeave call"}},
			known: personNamesFromContext([]db.PersonalEntry{{Subject: "ovren-kelvara-tumbrel-emzoran-wandel", Content: "the user's aunt"}}),
			want:  []string{"Ovren Kelvara Tumbrel Emzoran Wandel"},
		},
		{
			// Two capitalised interface words strung together must never come out as a name, known or not — an app's toolbar is never a person, however name-shaped the phrase reads.
			name:     "two interface words strung together are never a name",
			eps:      []db.Episode{{Title: "Meet - standup - Brave", ScreenText: "Raise Hand\nMore Options\nShare Screen"}},
			wantNone: true,
		},
		{
			// A name written in a script without case — Devanagari here, Tamil there — must reach the participant list the same way a Latin name does.
			name: "a caseless script name reaches the roster the way a Latin one does",
			eps: []db.Episode{
				{Title: "Meet - team-sync - Brave", ScreenText: "People (2)\nतोव्रिन\nYou\nMute\nLeave call"},
				{Title: "Meet - team-sync - Brave", ScreenText: "Participants (2)\nவெக்சில்\nYou\nMute\nLeave call"},
			},
			want: []string{"तोव्रिन", "வெக்சில்"},
			deny: []string{"Mute", "Leave", "You"},
		},
		{
			// A Hinglish meeting mixes a Latin name and a Devanagari name on the same line, and both must be read off it.
			name: "a Hinglish line yields both its Latin and Devanagari names",
			eps:  []db.Episode{{Title: "Meet - team-sync - Brave", ScreenText: "Yalven Pravik तोव्रिन\nMute\nLeave call"}},
			want: []string{"Yalven Pravik", "तोव्रिन"},
		},
		{
			// A line of chat text is not a roster line, and nothing on it — least of all the app's own words for mute, leave and participants — is a person on the call.
			name:     "CJK chat text with a colon is not a participant",
			eps:      []db.Episode{{Title: "Meet - team-sync - Brave", ScreenText: "泽姆纳: 静音 离开会议 参会者 我马上加入会议\n布拉克森: 好的 我们开始吧 请大家静音\nMute\nLeave call"}},
			wantNone: true,
		},
		{
			// A roster lists one name per line, and a name in a caseless script is written across two words there just as a Latin one is. It must arrive as the whole name rather than as its two words separately.
			name: "a caseless roster line is read as one whole name, not two halves",
			eps:  []db.Episode{{Title: "Meet - team-sync - Brave", ScreenText: "People (3)\nतोव्रिन मज़ेक\nYalven Pravik\nYou\nMute\nLeave call"}},
			want: []string{"तोव्रिन मज़ेक"},
			deny: []string{"तोव्रिन", "मज़ेक"},
		},
		{
			// Han is written without spaces between words, so a line of chat text, a toolbar button and a pane heading are each one unbroken run of letters exactly as a Chinese name would be. None of the three is a participant.
			name:     "CJK screen text without a colon is not a participant",
			eps:      []db.Episode{{Title: "Meet - team-sync - Brave", ScreenText: "我马上加入会议\n静音\n参会者"}},
			wantNone: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collectMeetingNames(c.eps, true, c.known)
			if c.wantNone && len(got) != 0 {
				t.Errorf("collectMeetingNames = %v, want none", got)
			}
			for _, w := range c.want {
				if !contains(got, w) {
					t.Errorf("got %v, want it to include %q", got, w)
				}
			}
			for _, d := range c.deny {
				if contains(got, d) {
					t.Errorf("got %v, must not include interface text %q", got, d)
				}
			}
			for _, w := range c.wantOnce {
				n := 0
				for _, g := range got {
					if g == w {
						n++
					}
				}
				if n != 1 {
					t.Errorf("got %v, want exactly one %q, got %d", got, w, n)
				}
			}
		})
	}
}

// A name written in a script without letter case has no "all caps" form, so the shouting check must leave it alone, while a Latin word in all caps is still interface furniture.
func TestLooksLikeName_ScriptsWithoutCaseAreNotShouting(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"वेक्सिल ज़ेल्ब्रक", true},
		{"ゼムナ ブラクセン", true},
		{"Emzor Wandel", true},
		{"MUTE", false},
		{"Share Screen 2", false},
	}
	for _, c := range cases {
		if got := looksLikeName(c.name); got != c.want {
			t.Errorf("looksLikeName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// On 2026-09-08 the prep for a Teams standup reached the window as a card titled "Before you join: Calendar | Daily Ai Sprint Standup | Microsoft Teams - Microphone recording - High memory usage - 1.1 GB", with the brief cut off at three lines and an Open button that opened nothing, because the notice named no place. The name is the one section of the title that is not furniture, the brief is filed as a conversation of Ora's own so it can be read in full, and the card opens that conversation.
func TestPrepMeeting_OpensAsAConversationNamedForTheMeeting(t *testing.T) {
	title := "Calendar | Daily Ai Sprint Standup | Microsoft Teams - Microphone recording - High memory usage - 1.1 GB"
	if got := meetingName(title); got != "Daily Ai Sprint Standup" {
		t.Fatalf("meetingName = %q, want the meeting's own name", got)
	}
	store := &fakeStore{episodes: []db.Episode{{Title: title, ScreenText: "Oskrev Thivan: joining now"}}}
	r, _, _ := newTestRecorder(t, store)
	if _, err := store.LogNote(context.Background(), "# Meeting minutes\n\nOskrev Thivan agreed to drop the Bill Eval Studio UI.", noteKind); err != nil {
		t.Fatal(err)
	}
	var got notifications
	r.notifyAt = got.addAt
	r.minutes = func(ctx context.Context, prompt string) (string, error) {
		return "Oskrev agreed to drop the Bill Eval Studio UI last time.", nil
	}

	r.prepMeeting()

	if len(store.conversations) != 1 || store.conversations[0] != "Before you join: Daily Ai Sprint Standup" {
		t.Fatalf("conversations = %v, want one named for the meeting", store.conversations)
	}
	if len(store.turns) != 1 || !strings.Contains(store.turns[0], "Bill Eval Studio") {
		t.Errorf("turns = %v, want the brief filed as Ora's turn", store.turns)
	}
	if len(got.sent) != 1 || !strings.HasPrefix(got.sent[0], "Before you join: Daily Ai Sprint Standup: ") || !strings.HasSuffix(got.sent[0], " @chats/1") {
		t.Errorf("notice = %v, want it to open the conversation in chats", got.sent)
	}
}
