package tracker_test

import (
	"context"
	"june/internal/tracker"
	"testing"
	"time"
)

// fake testing tracker
type mockTracker struct {
	responses []*tracker.Activity
	callCount int
}

func (m *mockTracker) GetActiveWindow() (*tracker.Activity, error) {
	if m.callCount >= len(m.responses) {
		return m.responses[len(m.responses)-1], nil
	}

	resp := m.responses[m.callCount]
	m.callCount++
	return resp, nil
}

func TestDaemon_DwellTimeAndBlocklist(t *testing.T) {
	mockEye := &mockTracker{
		responses: []*tracker.Activity{
			{App: "1Password.exe", Title: "Vault"}, // tick 1: blocked
			{App: "1Password.exe", Title: "Vault"}, // tick 2: blocked
			{App: "Code.exe", Title: "main.go"},    // tick 3: pending
			{App: "chrome.exe", Title: "Go docs"},  // tick 4: transient! Code.exe discarded, chrome.exe pending
			{App: "chrome.exe", Title: "Go docs"},  // tick 5
			{App: "chrome.exe", Title: "Go docs"},  // tick 6
			{App: "chrome.exe", Title: "Go docs"},  // tick 7: definitely emitted
			{App: "Code.exe", Title: "main.go"},    // tick 8: pending
			{App: "Code.exe", Title: "main.go"},    // tick 9
			{App: "Code.exe", Title: "main.go"},    // tick 10: definitely emitted
		},
	}

	eventChan := make(chan tracker.Activity, 10)

	// poll every 50ms, Dwell time requires 2 ticks (100ms)
	daemon := tracker.NewDaemon(mockEye, 50*time.Millisecond, 100*time.Millisecond, []string{"1Password.exe", "Taskmgr.exe"}, eventChan)
	daemon.SetCapturer(func() string { return "" }) // no-op: avoid real OCR in unit tests
	ctx, cancel := context.WithCancel(context.Background())

	go daemon.Start(ctx)

	var receivedEvents []tracker.Activity
	timeout := time.After(1 * time.Second)

CollectLoop:
	for {
		select {
		case ev := <-eventChan:
			receivedEvents = append(receivedEvents, ev)
		case <-timeout:
			cancel()
			break CollectLoop
		}
	}

	if len(receivedEvents) != 2 {
		t.Fatalf("Expected exactly 2 events (Chrome, Code), got %d: %+v", len(receivedEvents), receivedEvents)
	}

	if receivedEvents[0].App != "chrome.exe" {
		t.Errorf("Expected first event to be chrome.exe, got %s", receivedEvents[0].App)
	}
	if receivedEvents[1].App != "Code.exe" {
		t.Errorf("Expected second event to be Code.exe, got %s", receivedEvents[1].App)
	}
}

// A window the user comes straight back to must still be recorded. The tracker skips June's own window, an application on the blocklist, and a window nothing could name; each of those cleared the pending activity, and since the window the user returns to is the one the loop last saw, nothing then counts as changed — and only a pending activity can be emitted. So a window with a steady title was never recorded for as long as the user stayed in it. Hovering June fires this, which is the product's main interaction.
func TestDaemon_ASkippedWindowKeepsThePendingActivity(t *testing.T) {
	skips := map[string]tracker.Activity{
		"June's own window":           {App: "june", Title: "June"},
		"a blocked application":       {App: "1Password", Title: "Vault"},
		"a window nothing could name": {App: "Unknown", Title: "Unknown"},
	}

	for name, skip := range skips {
		t.Run(name, func(t *testing.T) {
			hover := skip
			code := &tracker.Activity{App: "Code", Title: "daemon.go"}
			mockEye := &mockTracker{responses: []*tracker.Activity{
				code,   // tick 1: the user is working, so this becomes the pending activity
				&hover, // tick 2: they glance at the window that is not their activity
				code,   // tick 3: back where they were, and the title has not changed
				code,   // tick 4: past the dwell time, so it must be recorded
			}}

			eventChan := make(chan tracker.Activity, 10)
			daemon := tracker.NewDaemon(mockEye, 20*time.Millisecond, 45*time.Millisecond, []string{"1Password"}, eventChan)
			daemon.SetCapturer(func() string { return "" })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go daemon.Start(ctx)

			var got []tracker.Activity
			timeout := time.After(400 * time.Millisecond)
		CollectLoop:
			for {
				select {
				case ev := <-eventChan:
					got = append(got, ev)
				case <-timeout:
					break CollectLoop
				}
			}

			if len(got) != 1 || got[0].App != "Code" {
				t.Fatalf("got %+v, want exactly one Code episode: the window the user was in was never recorded", got)
			}
		})
	}
}
