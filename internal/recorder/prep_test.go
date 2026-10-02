package recorder

import (
	"context"
	"strings"
	"testing"

	"june/internal/db"
)

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
			name:        "JUNE_NO_MEETING_PREP=1 is the alpha kill switch",
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

// On 2026-09-08 the prep for a Teams standup reached the window as a card titled "Before you join: Calendar | Daily Platform Sprint Standup | Microsoft Teams - Microphone recording - High memory usage - 1.1 GB", with the brief cut off at three lines and an Open button that opened nothing, because the notice named no place. The name is the one section of the title that is not furniture, the brief is filed as a conversation of June's own so it can be read in full, and the card opens that conversation.
func TestPrepMeeting_OpensAsAConversationNamedForTheMeeting(t *testing.T) {
	title := "Calendar | Daily Platform Sprint Standup | Microsoft Teams - Microphone recording - High memory usage - 1.1 GB"
	if got := meetingName(title); got != "Daily Platform Sprint Standup" {
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

	if len(store.conversations) != 1 || store.conversations[0] != "Before you join: Daily Platform Sprint Standup" {
		t.Fatalf("conversations = %v, want one named for the meeting", store.conversations)
	}
	if len(store.turns) != 1 || !strings.Contains(store.turns[0], "Bill Eval Studio") {
		t.Errorf("turns = %v, want the brief filed as June's turn", store.turns)
	}
	if len(got.sent) != 1 || !strings.HasPrefix(got.sent[0], "Before you join: Daily Platform Sprint Standup: ") || !strings.HasSuffix(got.sent[0], " @chats/1") {
		t.Errorf("notice = %v, want it to open the conversation in chats", got.sent)
	}
}
