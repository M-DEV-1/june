//go:build linux

package input

import (
	"errors"
	"testing"
	"time"
)

// Every successful event is followed by one wait of the given delay, matching internal/tracker/act_linux.go's per-event keyDelay pacing (the shell drops keys sent faster than a person could type).
func TestPaceEventsWaitsAfterEachEvent(t *testing.T) {
	var calls int
	var waits []time.Duration
	events := []func() error{
		func() error { calls++; return nil },
		func() error { calls++; return nil },
		func() error { calls++; return nil },
	}
	err := paceEvents(events, 12*time.Millisecond, func(d time.Duration) { waits = append(waits, d) })
	if err != nil {
		t.Fatalf("paceEvents: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if len(waits) != 3 {
		t.Fatalf("waits = %v, want 3 entries", waits)
	}
	for _, w := range waits {
		if w != 12*time.Millisecond {
			t.Fatalf("wait = %v, want 12ms", w)
		}
	}
}

// An event that fails stops the sequence: later events are never invoked and no trailing wait happens for the failed event.
func TestPaceEventsStopsOnError(t *testing.T) {
	boom := errors.New("boom")
	var calls int
	var waits int
	events := []func() error{
		func() error { calls++; return nil },
		func() error { calls++; return boom },
		func() error { calls++; return nil },
	}
	err := paceEvents(events, time.Millisecond, func(time.Duration) { waits++ })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (stop at the failing event)", calls)
	}
	if waits != 1 {
		t.Fatalf("waits = %d, want 1 (only after the first, successful event)", waits)
	}
}
