package proactive

import (
	"context"

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

// Every button that took tells the window so, whichever of Act's paths applied it: the window's card only drops its buttons when the notice comes back with its action set.
// Until 2026-09-23 only Done and the snoozes sent that event, so "Not happening" on the stale-item question and "Start recording" on the meeting offer left their buttons up, and a second press ran the action again.
func TestAct_TellsTheWindowTheCardIsDealtWith(t *testing.T) {
	SetNoticeAction("meeting", "record", func() error { return nil })
	t.Cleanup(func() { SetNoticeAction("meeting", "record", nil) })
	var sent []Notice
	SetNoticeSender(func(n Notice) bool { sent = append(sent, n); return true })
	t.Cleanup(func() { SetNoticeSender(nil) })

	n := Notice{Title: "Still open", Kind: staleNoticeKind, ID: "7"}
	answered, release := awaitAnswer(noticeKey(n), []string{"dropped"})
	defer release()

	s := &Scheduler{}
	for _, press := range []struct{ kind, id, title, action string }{
		{staleNoticeKind, "7", "Still open", "dropped"},
		{"meeting", "", "In a meeting?", "record"},
	} {
		sent = nil
		if err := s.Act(context.Background(), press.kind, press.id, press.title, "", press.action); err != nil {
			t.Fatalf("Act %s: %v", press.action, err)
		}
		if len(sent) != 1 || sent[0].Kind != press.kind || sent[0].Action != press.action {
			t.Errorf("%s pressed: window was sent %+v, want one %s notice with action %q", press.action, sent, press.kind, press.action)
		}
	}
	<-answered
}
