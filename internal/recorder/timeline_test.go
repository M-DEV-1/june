package recorder

import (
	"fmt"
	"testing"
	"time"

	"ora/internal/db"
)

// A four-hour meeting produces more screens than a prompt can carry. Truncating takes the first N and drops the rest, so the minutes are written as though the meeting ended early — the decisions, which land at the end, never reach the model. Sampling keeps the whole span.
func TestSampleTimeline_KeepsBothEndsOfALongMeeting(t *testing.T) {
	var eps []db.Episode
	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 800; i++ { // ~4 hours of distinct screens
		eps = append(eps, db.Episode{App: "Brave", Title: fmt.Sprintf("screen %d", i), CreatedAt: start.Add(time.Duration(i) * 18 * time.Second)})
	}

	got := sampleTimeline(eps, 120)

	if len(got) != 120 {
		t.Fatalf("got %d entries, want the cap of 120", len(got))
	}
	if got[0].Title != "screen 0" {
		t.Errorf("first entry is %q, want the meeting's start", got[0].Title)
	}
	if last := got[len(got)-1].Title; last != "screen 799" {
		t.Errorf("last entry is %q, want the meeting's end — truncation is what this replaces", last)
	}
}

// The tracker samples every couple of seconds and most samples repeat. Deduplicating before any cap is the point: capping first spends the budget on duplicates and then drops the end of the meeting.
func TestDedupeEpisodes_CollapsesRepeatsBeforeAnyCapApplies(t *testing.T) {
	same := db.Episode{App: "Brave", Title: "Value Chain", ScreenText: "the same screen"}
	eps := []db.Episode{same, same, same, {App: "Terminal", Title: "mdev1@mds-g15", ScreenText: "a build"}, same}

	got := dedupeEpisodes(eps)

	if len(got) != 3 {
		t.Fatalf("got %d, want 3 — three runs collapse to their first entries", len(got))
	}
	if got[2].App != "Brave" {
		t.Errorf("a screen returned to after leaving it is a new entry, got %q", got[2].App)
	}
}
