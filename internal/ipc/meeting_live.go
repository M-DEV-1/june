// meeting_live.go holds GET /meetings/live: what the meeting recording running right now looks like, before StopAndProcess has turned it into minutes.
package ipc

import (
	"context"
	"net/http"
	"time"

	"ora/internal/recorder"
)

// LiveRecorder is the part of *recorder.Recorder this route needs, narrowed so a test can fake it.
type LiveRecorder interface {
	LiveSnapshot(ctx context.Context) (recorder.LiveSnapshot, bool)
}

// The recorder is the LiveRecorder this route is written against; this says so at compile time, so a change to internal/recorder.Recorder's signature breaks here rather than in cmd/daemon.go.
var _ LiveRecorder = (*recorder.Recorder)(nil)

// LiveSegmentView is one line of transcript already known while the meeting is still running, on GET /meetings/live.
type LiveSegmentView struct {
	At      string `json:"at"`
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

// MeetingLiveView is GET /meetings/live's body. TranscribedThrough is "" until SegmentsSoFar carries real transcription.
type MeetingLiveView struct {
	StartedAt          string            `json:"started_at"`
	Window             string            `json:"window"`
	Participants       []string          `json:"participants"`
	SegmentsSoFar      []LiveSegmentView `json:"segments_so_far"`
	TranscribedThrough string            `json:"transcribed_through"`
	Note               string            `json:"note"`
}

// MeetingLive handles GET /meetings/live: the snapshot of the meeting recording running right now, or 204 when none is.
func MeetingLive(rec LiveRecorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap, ok := rec.LiveSnapshot(r.Context())
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		segs := make([]LiveSegmentView, 0, len(snap.SegmentsSoFar))
		for _, s := range snap.SegmentsSoFar {
			segs = append(segs, LiveSegmentView{At: s.At.UTC().Format(time.RFC3339), Speaker: s.Speaker, Text: s.Text})
		}
		participants := snap.Participants
		if participants == nil {
			participants = []string{}
		}
		view := MeetingLiveView{
			StartedAt:     snap.StartedAt.UTC().Format(time.RFC3339),
			Window:        snap.Window,
			Participants:  participants,
			SegmentsSoFar: segs,
			Note:          snap.Note,
		}
		if !snap.TranscribedThrough.IsZero() {
			view.TranscribedThrough = snap.TranscribedThrough.UTC().Format(time.RFC3339)
		}
		writeJSON(w, view)
	}
}
