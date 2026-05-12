package tracker_test

import (
	"context"
	"ora/internal/tracker"
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

func TestDaemon_OnlyLogOnWindowsChange(t *testing.T) {
	mockEye := &mockTracker{
		responses: []*tracker.Activity{
			{App: "Code.exe", Title: "main.go - VSCode"},
			{App: "Code.exe", Title: "main.go - VSCode"}, // to ignore
			{App: "chrome.exe", Title: "Go docs"},        // to log
		},
	}

	eventChan := make(chan tracker.Activity, 10)

	daemon := tracker.NewDaemon(mockEye, 10*time.Millisecond, eventChan)
	ctx, cancel := context.WithCancel(context.Background())

	go daemon.Start(ctx)

	time.Sleep(50 * time.Millisecond)
	cancel()

	close(eventChan)

	// verification
	var receivedEvents []tracker.Activity
	for ev := range eventChan {
		receivedEvents = append(receivedEvents, ev)
	}

	if len(receivedEvents) != 2 {
		t.Fatalf("Expected 2 events, got %d: %+v", len(receivedEvents), receivedEvents)
	}

	if receivedEvents[1].App != "chrome.exe" {
		t.Errorf("Expected second event to be chrome, got %s", receivedEvents[1].App)
	}

	t.Logf("Capture verified: Tracked %d distinct window changes.", len(receivedEvents))
}
