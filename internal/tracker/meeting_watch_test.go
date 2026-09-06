package tracker

import (
	"context"
	"testing"
	"time"
)

// The meeting watcher puts activities on the same channel the tick loop does, without a tick loop's gate in front of them: it checked the blocklist and nothing else, so Ora's own window and a window nothing could name went straight to the store, and an activity with no application name at all reached WriteEpisode. Every case below is one the tick loop already refuses, and the watcher must refuse it too.
func TestWatchMeetingWindow_SkipsWhatTheTickLoopSkips(t *testing.T) {
	cases := map[string]struct {
		app, title, text string
		blocklist        []string
		want             bool // whether an activity should reach the channel
	}{
		"a call in a blocked application": {
			app: "zoom", title: "Zoom Meeting", text: "the standup", blocklist: []string{"zoom"}, want: false,
		},
		"Ora's own window matching the meeting words": {
			app: "ora", title: "Zoom Meeting", text: "a summary of the call", want: false,
		},
		"a window nothing could name": {
			app: "", title: "", text: "", want: false,
		},
		"an ordinary call": {
			app: "teams", title: "Chat | Priya | Microsoft Teams", text: "who is presenting", want: true,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			events := make(chan Activity, 1)
			d := NewDaemon(nil, time.Second, time.Second, c.blocklist, events)
			d.meeting = func() (string, string, string, bool) { return c.app, c.title, c.text, c.app != "" || c.title != "" }
			d.meetingEvery = 5 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go d.watchMeetingWindow(ctx)

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

// A call window with no application name behind it must still be filed under a name, because an activity whose App and Title are both empty reaches the store as an episode with nothing to say what it was. Normalize is what the tick loop uses for that, and the watcher was the one path that skipped it.
func TestWatchMeetingWindow_NamesAWindowWithNoApplication(t *testing.T) {
	events := make(chan Activity, 1)
	d := NewDaemon(nil, time.Second, time.Second, nil, events)
	d.meeting = func() (string, string, string, bool) { return "", "Google Meet", "the retro", true }
	d.meetingEvery = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.watchMeetingWindow(ctx)

	select {
	case ev := <-events:
		if ev.App != "Unknown" || ev.Title != "Google Meet" {
			t.Fatalf("the watcher emitted app %q title %q, want the normalized pair", ev.App, ev.Title)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("the watcher emitted nothing for a call window with no application name")
	}
}
