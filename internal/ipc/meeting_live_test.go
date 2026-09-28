package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"june/internal/recorder"
)

// fakeLiveRecorder stands in for *recorder.Recorder: it answers LiveSnapshot with whatever a test seeded.
type fakeLiveRecorder struct {
	snap recorder.LiveSnapshot
	ok   bool
}

func (f *fakeLiveRecorder) LiveSnapshot(context.Context) (recorder.LiveSnapshot, bool) {
	return f.snap, f.ok
}

// While a meeting is recording, the route reports the snapshot as JSON, and never a null list for participants or segments even when the recorder has none yet.
func TestMeetingLive_WhileRecording(t *testing.T) {
	started := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	snap := recorder.LiveSnapshot{
		StartedAt:     started,
		Window:        "Meet - weekly sync - Brave",
		Participants:  []string{"Vexil Quorin"},
		SegmentsSoFar: []recorder.LiveSegment{},
		Note:          "transcribed at the end",
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/meetings/live", nil)
	MeetingLive(&fakeLiveRecorder{snap: snap, ok: true}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got MeetingLiveView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body)
	}
	if got.StartedAt != started.Format(time.RFC3339) {
		t.Errorf("started_at = %q, want %q", got.StartedAt, started.Format(time.RFC3339))
	}
	if got.Window != snap.Window {
		t.Errorf("window = %q, want %q", got.Window, snap.Window)
	}
	if len(got.Participants) != 1 || got.Participants[0] != "Vexil Quorin" {
		t.Errorf("participants = %v, want [Vexil Quorin]", got.Participants)
	}
	if got.SegmentsSoFar == nil || len(got.SegmentsSoFar) != 0 {
		t.Errorf("segments_so_far = %v, want an empty, non-null list", got.SegmentsSoFar)
	}
	if got.TranscribedThrough != "" {
		t.Errorf("transcribed_through = %q, want empty since nothing has been transcribed yet", got.TranscribedThrough)
	}
	if got.Note != snap.Note {
		t.Errorf("note = %q, want %q", got.Note, snap.Note)
	}
}
