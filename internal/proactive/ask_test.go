package proactive

import (
	"testing"
	"time"
)

// A question the window took is answered on the card, and no desktop banner is posted at all. This is the rule the whole notice surface is built on: one surface, not two, or the user is asked the same thing twice.
func TestAsk_AnsweredOnTheCardPostsNoBanner(t *testing.T) {
	n := Notice{Title: "In a meeting?", Body: "Brave is using your microphone.", Kind: "meeting", ID: "1", Actions: []Action{{Key: "record", Label: "Start recording"}}}
	SetNoticeSender(func(Notice) bool { return true })
	defer SetNoticeSender(nil)

	bannered := false
	banner := func(title, body string, actions []string) (string, error) {
		bannered = true
		return "", nil
	}

	// The card is pressed once the waiter is registered, which Ask does before it offers the notice.
	go func() {
		for i := 0; i < 200; i++ {
			if deliverAnswer(noticeKey(n), "record") {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	if got := Ask(n, time.Second, banner, nil); got != "record" {
		t.Errorf("Ask returned %q, want the key pressed on the card", got)
	}
	if bannered {
		t.Error("a desktop banner was posted for a question the window already took")
	}
}

// With no window listening the question falls back to the desktop banner, carrying its own buttons as notify-send spells them.
func TestAsk_FallsBackToTheBannerWithNoWindow(t *testing.T) {
	n := Notice{Title: "In a meeting?", Body: "Brave is using your microphone.", Kind: "meeting", ID: "2", Actions: []Action{{Key: "record", Label: "Start recording"}}}
	SetNoticeSender(func(Notice) bool { return false })
	defer SetNoticeSender(nil)

	var gotActions []string
	banner := func(title, body string, actions []string) (string, error) {
		gotActions = actions
		return "record", nil
	}

	if got := Ask(n, time.Second, banner, nil); got != "record" {
		t.Errorf("Ask returned %q, want the banner's answer", got)
	}
	if len(gotActions) != 1 || gotActions[0] != "record=Start recording" {
		t.Errorf("banner actions = %v, want one \"record=Start recording\"", gotActions)
	}
}

// A question's own buttons keep working after the question has timed out. The goroutine that asked is gone, so the press falls through to Act's four-action switch, which knows only Done and the snoozes — every other key was refused there and the card said "Could not do that" while nothing was tried. Ask registers what its buttons do so this cannot happen to a question that has not been given the registration by hand.
func TestAsk_ButtonsStillWorkAfterTheQuestionGaveUp(t *testing.T) {
	n := Notice{Title: "Still open", Kind: "stale", ID: "77", Actions: []Action{{Key: "dropped", Label: "Not happening"}, {Key: "low", Label: "Not urgent"}}}
	SetNoticeSender(func(Notice) bool { return true })
	defer SetNoticeSender(nil)

	applied := ""
	if got := Ask(n, time.Millisecond, nil, func(key string) error { applied = key; return nil }); got != "" {
		t.Fatalf("Ask returned %q, want the empty answer of a question nobody pressed", got)
	}

	s := &Scheduler{}
	if err := s.Act(t.Context(), "stale", "77", "Still open", "", "dropped"); err != nil {
		t.Fatalf("pressing the card's own button after the question gave up: %v", err)
	}
	if applied != "dropped" {
		t.Errorf("applied %q, want the button that was pressed", applied)
	}
}
