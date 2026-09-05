package recorder

import (
	"context"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// SetBrain installs the brain the daemon meters against its shared Gemini quota; once installed, defaultBrain must call it rather than building its own unmetered brain from config.
func TestDefaultBrain_UsesInstalledBrain(t *testing.T) {
	r := New(context.Background(), t.TempDir(), &fakeStore{}, "")
	<-r.swept
	var gotPrompt string
	r.SetBrain(func(ctx context.Context, prompt string) (string, error) {
		gotPrompt = prompt
		return "installed brain replied", nil
	})

	out, err := r.defaultBrain(context.Background(), "a meeting prompt")
	if err != nil {
		t.Fatalf("defaultBrain returned an error with a brain installed: %v", err)
	}
	if out != "installed brain replied" {
		t.Errorf("defaultBrain returned %q, want the installed brain's reply", out)
	}
	if gotPrompt != "a meeting prompt" {
		t.Errorf("installed brain got prompt %q, want the one defaultBrain was called with", gotPrompt)
	}
}

// The minutes are the only place an action item's owner is ever decided, and the tasks page is built from the "Me" items alone, so the prompt has to name all three owners it will accept.
func TestMinutesInstruction_NamesTheThreeOwners(t *testing.T) {
	for _, want := range []string{`"Me", for anything the [me] speaker owes`, `One other person's name`, `"Owner unclear"`, `"I'll send the deck" are all "Me"`} {
		if !strings.Contains(minutesInstruction, want) {
			t.Errorf("the minutes prompt no longer says %q", want)
		}
	}
}

// The prompt states the meeting's own clock in local time, so the screen timeline under it has to be on the same clock: episodes come back from SQLite in UTC, and a row printed straight from CreatedAt reads hours away from the meeting it belongs to.
func TestBuildPrompt_StampsTheTimelineInLocalTime(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("IST", 5*3600+30*60)
	t.Cleanup(func() { time.Local = saved })

	at := time.Date(2026, 9, 5, 10, 47, 0, 0, time.UTC)
	store := &fakeStore{episodes: []db.Episode{{App: "Brave", Title: "Meet - abc-defg-hij", ScreenText: "Priya Shah (Presenting)", CreatedAt: at}}}
	r, _, _ := newTestRecorder(t, store)

	prompt := r.buildPrompt(context.Background(), "[me] hello", at.Add(-time.Minute), at.Add(time.Minute))

	if !strings.Contains(prompt, "16:17  Brave — Meet - abc-defg-hij") {
		t.Errorf("timeline row is not on the user's clock (want 16:17):\n%s", prompt)
	}
	if strings.Contains(prompt, "10:47") {
		t.Errorf("timeline row is still stamped in UTC:\n%s", prompt)
	}
}
