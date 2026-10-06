package tracker

import (
	"context"
	"testing"
	"time"
)

// The meeting watcher puts activities on the same channel the tick loop does, without a tick loop's gate in front of them: it checked the blocklist and nothing else, so June's own window and a window nothing could name went straight to the store, and an activity with no application name at all reached WriteEpisode. Every case below is one the tick loop already refuses, and the watcher must refuse it too.
func TestWatchMeetingWindow_SkipsWhatTheTickLoopSkips(t *testing.T) {
	cases := map[string]struct {
		app, title, text string
		blocklist        []string
		want             bool // whether an activity should reach the channel
	}{
		"a call in a blocked application": {
			app: "zoom", title: "Zoom Meeting", text: "the standup", blocklist: []string{"zoom"}, want: false,
		},
		"June's own window matching the meeting words": {
			app: "june", title: "Zoom Meeting", text: "a summary of the call", want: false,
		},
		"a window nothing could name": {
			app: "", title: "", text: "", want: false,
		},
		"an ordinary call": {
			app: "teams", title: "Chat | Vexil | Microsoft Teams", text: "who is presenting", want: true,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			events := make(chan Activity, 1)
			d := NewDaemon(nil, time.Second, time.Second, c.blocklist, events)
			d.meeting = func() (string, string, string, bool) { return c.app, c.title, c.text, c.app != "" || c.title != "" }
			d.meetingEvery = 5 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { d.watchMeetingWindow(ctx); close(done) }()
			defer func() { cancel(); <-done }()

			select {
			case ev := <-events:
				if !c.want {
					t.Fatalf("the watcher emitted %+v, which the tick loop would have skipped", ev)
				}
				if ev.App != "teams" || ev.ScreenText != "who is presenting" {
					t.Fatalf("the watcher emitted %+v, want the call's own window", ev)
				}
			case <-time.After(300 * time.Millisecond):
				if c.want {
					t.Fatal("the watcher emitted nothing for a call it should have recorded")
				}
			}
		})
	}
}
