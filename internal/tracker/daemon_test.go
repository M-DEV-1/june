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
