package tracker

import (
	"context"
	"testing"
	"time"
)

// The text a capture reads belongs to whatever had focus while it ran, not to the window the tick loop decided on up to three seconds earlier. skipReason ran against the earlier window, so a blocked application's text could be filed under the one before it: dwell in Brave, Alt-Tab to KeePassXC, and the vault's contents were written as app=Brave with the blocklist never consulted. noteWindow is what the tick loop calls on every poll, so a capture asking where the user is now gets the loop's own latest answer.
func TestCaptureAndEmit_DropsTextReadAfterTheUserMovedOn(t *testing.T) {
	t.Run("the window changed under the capture", func(t *testing.T) {
		events := make(chan Activity, 1)
		d := NewDaemon(nil, time.Second, time.Second, []string{"keepassxc"}, events)
		d.noteWindow(Activity{App: "Brave", Title: "GitHub"})
		capture := func(Activity) captureOut {
			d.noteWindow(Activity{App: "keepassxc", Title: "Passwords"})
			return captureOut{text: "the master password list", frames: [][]byte{{1, 2, 3}}}
		}

		d.captureAndEmit(context.Background(), Activity{App: "Brave", Title: "GitHub"}, capture, false)
		ev := <-events

		if ev.App != "Brave" {
			t.Fatalf("episode app = %q, want the window the loop decided on", ev.App)
		}
		if ev.ScreenText != "" {
			t.Fatalf("episode screen text = %q, want nothing: it was read out of a window this episode does not name", ev.ScreenText)
		}
		if len(ev.ImageJPEG) != 0 {
			t.Fatal("the frame grabbed after the user moved on was kept")
		}
	})

	t.Run("the window stayed put", func(t *testing.T) {
		events := make(chan Activity, 1)
		d := NewDaemon(nil, time.Second, time.Second, nil, events)
		d.noteWindow(Activity{App: "Brave", Title: "GitHub"})
		capture := func(Activity) captureOut { return captureOut{text: "a pull request"} }

		d.captureAndEmit(context.Background(), Activity{App: "Brave", Title: "GitHub"}, capture, false)
		ev := <-events

		if ev.ScreenText != "a pull request" {
			t.Fatalf("episode screen text = %q, want the capture kept when focus never moved", ev.ScreenText)
		}
	})
}
