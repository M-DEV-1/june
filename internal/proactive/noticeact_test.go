package proactive

import (
	"context"
	"errors"
	"testing"
)

// A notice's own button has to work when it is pressed, not only while some goroutine happens to still be parked waiting for it.
// Until this, "Start recording" answered a goroutine inside askToRecord and nothing else: a press that arrived after that goroutine gave up — or after a daemon restart, or on a card the window had drawn twice — fell through to the four-action switch, which does not know the word "record", and the card said "Could not do that" while no recording was ever attempted.
func TestAct_RunsTheNoticesOwnActionWithNoOneWaiting(t *testing.T) {
	started := false
	SetNoticeAction("meeting", "record", func() error { started = true; return nil })
	t.Cleanup(func() { SetNoticeAction("meeting", "record", nil) })

	s := &Scheduler{}
	if err := s.Act(context.Background(), "meeting", "", "In a meeting?", "Brave is using your microphone.", "record"); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if !started {
		t.Error("the press did not start the recording, which is the only thing that button is for")
	}
}

// The action's own failure reaches the user rather than being reported as a bad button.
func TestAct_ReportsWhatTheActionFailedWith(t *testing.T) {
	SetNoticeAction("meeting", "record", func() error { return errors.New("no microphone") })
	t.Cleanup(func() { SetNoticeAction("meeting", "record", nil) })

	s := &Scheduler{}
	err := s.Act(context.Background(), "meeting", "", "In a meeting?", "", "record")
	if err == nil || errors.Is(err, ErrBadNoticeAction) {
		t.Errorf("Act = %v, want the reason the recording would not start", err)
	}
}

// A goroutine that is waiting still gets the answer, so the watcher keeps its own bookkeeping and nothing starts twice.
func TestAct_PrefersTheWaitingGoroutine(t *testing.T) {
	ran := false
	SetNoticeAction("meeting", "record", func() error { ran = true; return nil })
	t.Cleanup(func() { SetNoticeAction("meeting", "record", nil) })

	n := Notice{Title: "In a meeting?", Kind: "meeting"}
	answered, release := awaitAnswer(noticeKey(n), []string{"record"})
	defer release()

	s := &Scheduler{}
	if err := s.Act(context.Background(), "meeting", "", n.Title, "", "record"); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if ran {
		t.Error("the fallback ran while a goroutine was waiting, so the recording would start twice")
	}
	if got := <-answered; got != "record" {
		t.Errorf("the waiting goroutine got %q", got)
	}
}

// A word no notice registered is still refused, so a stray press cannot reach anything.
func TestAct_StillRefusesAnActionNobodyRegistered(t *testing.T) {
	s := &Scheduler{}
	if err := s.Act(context.Background(), "meeting", "", "In a meeting?", "", "juggle"); !errors.Is(err, ErrBadNoticeAction) {
		t.Errorf("Act = %v, want it refused", err)
	}
}
