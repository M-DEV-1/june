package tracker_test

import (
	"context"
	"ora/internal/tracker"
	"testing"
	"time"
)

// The rule for Ora's own window: the app name decides, except for the XWayland frame process, which is only Ora when its title says so.
func TestIsOraWindow(t *testing.T) {
	tests := []struct {
		app, title string
		want       bool
	}{
		{"ora", "Ora", true},
		{"Ora", "anything", true},
		{" ora ", "", true},
		{"mutter-x11-frames", "Ora", true},
		{"mutter-x11-frames", "Slack", false},
		{"gnome-terminal", "ora", false},
		{"Google Chrome", "ora — mail", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := tracker.IsOraWindow(tt.app, tt.title); got != tt.want {
			t.Errorf("IsOraWindow(%q, %q) = %v, want %v", tt.app, tt.title, got, tt.want)
		}
	}
}

// The capture loop must never emit Ora's own window, so it reaches neither the episode store nor the live activity buffer, both of which are fed from this one channel.
func TestDaemon_SkipsOraOwnWindow(t *testing.T) {
	mockEye := &mockTracker{
		responses: []*tracker.Activity{
			{App: "ora", Title: "Ora"},
			{App: "ora", Title: "Ora"},
			{App: "ora", Title: "Ora"},
			{App: "mutter-x11-frames", Title: "Ora"},
			{App: "mutter-x11-frames", Title: "Ora"},
			{App: "mutter-x11-frames", Title: "Ora"},
			{App: "Code", Title: "reads.go"},
			{App: "Code", Title: "reads.go"},
			{App: "Code", Title: "reads.go"},
			{App: "Code", Title: "reads.go"},
		},
	}

	eventChan := make(chan tracker.Activity, 10)
	daemon := tracker.NewDaemon(mockEye, 20*time.Millisecond, 40*time.Millisecond, nil, eventChan)
	daemon.SetCapturer(func() string { return "" })
	ctx, cancel := context.WithCancel(context.Background())
	go daemon.Start(ctx)

	var got []tracker.Activity
	timeout := time.After(700 * time.Millisecond)
CollectLoop:
	for {
		select {
		case ev := <-eventChan:
			got = append(got, ev)
		case <-timeout:
			cancel()
			break CollectLoop
		}
	}

	if len(got) != 1 || got[0].App != "Code" {
		t.Fatalf("got %+v, want exactly one Code episode and no Ora ones", got)
	}
}
