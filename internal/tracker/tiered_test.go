package tracker

import (
	"testing"
	"time"
)

func TestShouldUseVision(t *testing.T) {
	cases := []struct {
		name            string
		textLen         int
		visionEnabled   bool
		mediaActive     bool
		sinceLastVision time.Duration
		want            bool
	}{
		{"rich accessibility text -> no vision", 5000, true, false, time.Hour, false},
		{"thin text, vision ready -> vision", 10, true, false, time.Hour, true},
		{"empty text, vision ready -> vision", 0, true, false, time.Hour, true},
		{"thin text but vision disabled -> no", 10, false, false, time.Hour, false},
		{"thin text but rate-limited -> no", 10, true, false, time.Second, false},
		{"exactly at threshold -> no vision", thinTextThreshold, true, false, time.Hour, false},
		{"just below threshold -> vision", thinTextThreshold - 1, true, false, time.Hour, true},

		// mediaActive (MPRIS "Playing") bypasses the text-length gate: a browser tab playing video/a call returns lots of chrome text but describes nothing about what's on screen.
		{"media playing, rich chrome text, ready -> vision fires anyway", 5000, true, true, time.Hour, true},
		{"media not playing, rich text -> still no vision", 5000, true, false, time.Hour, false},
		{"media playing, thin text, ready -> vision", 10, true, true, time.Hour, true},
		{"media playing but vision disabled -> no", 5000, false, true, time.Hour, false},
		{"media playing but rate-limited -> no", 5000, true, true, time.Second, false},
	}
	for _, c := range cases {
		if got := shouldUseVision(c.textLen, c.visionEnabled, c.mediaActive, c.sinceLastVision); got != c.want {
			t.Errorf("%s: shouldUseVision=%v want %v", c.name, got, c.want)
		}
	}
}

func TestIsVisionWorthy(t *testing.T) {
	cases := []struct {
		name string
		act  Activity
		want bool
	}{
		{"real browser", Activity{App: "Brave", Title: "GitHub - ora"}, true},
		{"fullscreen video", Activity{App: "mpv", Title: "movie.mkv"}, true},
		{"game", Activity{App: "steam_app_12345", Title: "Hades"}, true},
		{"bare desktop (unknown app)", Activity{App: "Unknown", Title: "Unknown"}, false},
		{"empty app", Activity{App: "", Title: ""}, false},
		{"gnome shell", Activity{App: "gnome-shell", Title: ""}, false},
		{"gjs shell", Activity{App: "gjs", Title: "Unknown"}, false},
		{"plasmashell", Activity{App: "plasmashell", Title: "Desktop"}, false},
		{"mutter compositor", Activity{App: "Mutter", Title: ""}, false},
	}
	for _, c := range cases {
		if got := isVisionWorthy(c.act); got != c.want {
			t.Errorf("%s: isVisionWorthy=%v want %v", c.name, got, c.want)
		}
	}
}

func TestSightText_IgnoresEmptyAndDoesNotNeedA11y(t *testing.T) {
	s := Sight{UserActivity: "watching a lecture", VisibleText: []string{"Week 4: backprop", "loss: 0.12"}}
	if got := s.Text(); got != "watching a lecture\nWeek 4: backprop\nloss: 0.12" {
		t.Fatalf("got %q", got)
	}
	if (Sight{}).Text() != "" {
		t.Fatal("empty sight should not invent a11y")
	}
}

func TestDiff(t *testing.T) {
	last := "old"
	if got := diff(&last, "old"); got != "" {
		t.Errorf("unchanged should return empty, got %q", got)
	}
	if got := diff(&last, ""); got != "" {
		t.Errorf("empty should return empty, got %q", got)
	}
	if got := diff(&last, "new"); got != "new" {
		t.Errorf("changed should return new text, got %q", got)
	}
	if last != "new" {
		t.Errorf("diff should update last in place, got %q", last)
	}
}
