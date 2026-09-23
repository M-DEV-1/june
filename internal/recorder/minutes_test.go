package recorder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ora/internal/db"
)

// The prompt states the meeting's own clock in local time, so the screen timeline under it has to be on the same clock: episodes come back from SQLite in UTC, and a row printed straight from CreatedAt reads hours away from the meeting it belongs to.
func TestBuildPrompt_StampsTheTimelineInLocalTime(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("IST", 5*3600+30*60)
	t.Cleanup(func() { time.Local = saved })

	at := time.Date(2026, 9, 5, 10, 47, 0, 0, time.UTC)
	store := &fakeStore{episodes: []db.Episode{{App: "Brave", Title: "Meet - abc-defg-hij", ScreenText: "Vexil Quorin (Presenting)", CreatedAt: at}}}
	r, _, _ := newTestRecorder(t, store)

	prompt := r.buildPrompt(context.Background(), "[me] hello", at.Add(-time.Minute), at.Add(time.Minute))

	if !strings.Contains(prompt, "16:17  Brave — Meet - abc-defg-hij") {
		t.Errorf("timeline row is not on the user's clock (want 16:17):\n%s", prompt)
	}
	if strings.Contains(prompt, "10:47") {
		t.Errorf("timeline row is still stamped in UTC:\n%s", prompt)
	}
}

// TestCoveredHeadingMatchesFrontend checks that app/src/next/task-about.tsx still contains the CoveredHeading text, since that file reads the same heading out of rendered minutes to show a task's originating meeting summary, and the two are typed independently in Go and TypeScript.
func TestCoveredHeadingMatchesFrontend(t *testing.T) {
	path := filepath.Join("..", "..", "app", "src", "next", "task-about.tsx")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !strings.Contains(string(data), CoveredHeading) {
		t.Errorf("%s no longer contains %q; update its COVERED constant to match internal/recorder.CoveredHeading", path, CoveredHeading)
	}
}

// A mic stream that carried no speech used to reach the model as a transcript with nothing but [call] lines in it, and the model read that as a person who sat through the meeting without speaking: "the user did not speak in this meeting" on 16 September 2026, in a standup where mic.wav holds his whole update at a fortieth of normal level. The prompt has to say the microphone failed, because the transcript alone cannot tell the two apart.
func TestBuildPrompt_SaysWhenTheMicrophoneCapturedNothing(t *testing.T) {
	r := &Recorder{store: &fakeStore{}}
	callOnly := "[00:00:02] [call] friday works\n"
	prompt := r.buildPrompt(context.Background(), callOnly, time.Now(), time.Now().Add(time.Hour))
	if !strings.Contains(prompt, micCapturedNothing) {
		t.Errorf("a transcript with no [me] line should carry the microphone warning:\n%s", prompt)
	}

	both := "[00:00:00] [me] shall we ship friday\n\n[00:00:02] [call] friday works\n"
	if prompt := r.buildPrompt(context.Background(), both, time.Now(), time.Now().Add(time.Hour)); strings.Contains(prompt, micCapturedNothing) {
		t.Error("a transcript with [me] lines in it should not carry the microphone warning")
	}
}
